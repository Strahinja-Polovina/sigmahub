package container

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/agent/internal/dsd"
)

// rolloutDocker is the slice of the Docker client the blue-green swap needs.
// Extracted so the ordering invariant (never cut — the new container is created
// and healthy BEFORE the old is drained) is unit-testable with a fake.
type rolloutDocker interface {
	ContainerList(ctx context.Context) ([]ContainerState, error)
	ContainerInspect(ctx context.Context, name string) (ContainerState, bool, error)
	ContainerCreate(ctx context.Context, name string, body any) (string, error)
	ContainerStart(ctx context.Context, id string) error
	ContainerStop(ctx context.Context, id string, grace time.Duration) error
	ContainerRemove(ctx context.Context, id string, force bool) error
}

// Prober reports whether a container at ip is healthy per the probe. Swapped in
// tests; defaultProbe is the real HTTP/TCP implementation.
type Prober func(ctx context.Context, ip string, p HealthProbe) error

// rolloutName is the per-generation container name: <baseName>-<generation>,
// where baseName is the canonical sigmahub-<res> the control plane renders. Two
// generations of the same resource coexist during a swap, so each needs a
// distinct name; Traefik groups them by their (stable, resource-derived) router
// labels, not the container name.
func rolloutName(baseName, generation string) string {
	if generation == "" {
		return baseName
	}
	return baseName + "-" + generation
}

// olderGenerations returns the managed containers in the same (resource, service)
// group whose name is not keepName — the previous generations to drain after the
// new one is healthy. service is "" for a single-container app; a Compose service
// scopes to its own generations so one service's swap never touches another's.
func olderGenerations(list []ContainerState, resourceID, service, keepName string) []ContainerState {
	var out []ContainerState
	for _, c := range list {
		if c.Labels[LabelResourceID] == resourceID && c.Labels[LabelService] == service && c.Name != keepName {
			out = append(out, c)
		}
	}
	return out
}

// defaultProbe is the real health probe: an HTTP GET expecting a 2xx, or a bare
// TCP dial. A single attempt — gateHealthy retries it until the deadline. Type
// "none" (a portless service, e.g. a Compose worker) has nothing to probe —
// running is the readiness signal.
func defaultProbe(ctx context.Context, ip string, p HealthProbe) error {
	if p.Type == "none" {
		return nil
	}
	port := p.Port
	if port == 0 {
		port = 80
	}
	addr := net.JoinHostPort(ip, strconv.Itoa(port))
	if p.Type == "tcp" {
		d := net.Dialer{Timeout: 3 * time.Second}
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return err
		}
		_ = conn.Close()
		return nil
	}
	// http
	path := p.Path
	if path == "" {
		path = "/"
	}
	reqCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return fmt.Errorf("health check %s returned %d", path, resp.StatusCode)
	}
	return nil
}

// gateHealthy polls the container until the probe passes or the deadline lapses.
// Re-inspects each round for the (stable, once-running) container IP.
func gateHealthy(ctx context.Context, dk rolloutDocker, probe Prober, name string, p HealthProbe) error {
	interval := time.Duration(p.IntervalSec) * time.Second
	if interval <= 0 {
		interval = 2 * time.Second
	}
	timeout := time.Duration(p.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		cur, ok, err := dk.ContainerInspect(ctx, name)
		if err != nil {
			return err
		}
		if !ok || !cur.Running {
			lastErr = fmt.Errorf("container exited before becoming healthy")
		} else if p.Type == "none" {
			return nil // portless service: running IS the readiness signal
		} else if cur.IP == "" {
			lastErr = fmt.Errorf("container has no address yet")
		} else if err := probe(ctx, cur.IP, p); err != nil {
			lastErr = err
		} else {
			return nil // healthy
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("health gate timed out after %s: %w", timeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// drainOld gracefully stops then removes every previous generation of the
// (resource, service) group. A graceful stop lets Traefik drop the backend and
// in-flight requests complete before the container goes away.
func drainOld(ctx context.Context, dk rolloutDocker, resourceID, service, keepName string, log logf) error {
	list, err := dk.ContainerList(ctx)
	if err != nil {
		return err
	}
	for _, c := range olderGenerations(list, resourceID, service, keepName) {
		if err := dk.ContainerStop(ctx, c.ID, 15*time.Second); err != nil {
			log("drain: stop old", "container", c.Name, "err", err)
		}
		if err := dk.ContainerRemove(ctx, c.ID, true); err != nil {
			log("drain: remove old", "container", c.Name, "err", err)
		}
	}
	return nil
}

type logf func(msg string, args ...any)

// imageRetainer is the image slice keep-last-N retention needs. Extracted so the
// retention policy is unit-testable with a fake.
type imageRetainer interface {
	ImageList(ctx context.Context, reference string) ([]ImageSummary, error)
	ImageRemove(ctx context.Context, ref string, force bool) error
}

// defaultImageRetention is how many built images per resource are kept so a
// rebuild-free rollback always has a target; older images are pruned to bound
// disk use.
const defaultImageRetention = 10

// retainImages keeps the newest `keep` images under a repo prefix (e.g.
// "sigmahub/<res>:" or a Compose service's "sigmahub/<res>-<svc>:") and removes
// older tags, never removing one in `inUse` (a running generation — the image
// currently serving must survive). An in-use tag does not consume a keep slot, so
// `keep` historical images are retained BEYOND the running one. Best-effort: a
// removal failure is logged, never fatal — a deploy must not fail because an old
// image couldn't be pruned.
func retainImages(ctx context.Context, ir imageRetainer, prefix string, keep int, inUse map[string]bool, log logf) {
	if keep <= 0 {
		keep = defaultImageRetention
	}
	if prefix == "" {
		return
	}
	imgs, err := ir.ImageList(ctx, prefix+"*")
	if err != nil {
		log("retain: list images", "prefix", prefix, "err", err)
		return
	}
	// Collect this resource's tags newest-first (ImageList sorts by Created desc).
	var tags []string
	seen := map[string]bool{}
	for _, im := range imgs {
		for _, t := range im.RepoTags {
			if strings.HasPrefix(t, prefix) && !seen[t] {
				seen[t] = true
				tags = append(tags, t)
			}
		}
	}
	kept := 0
	for _, t := range tags {
		if inUse[t] {
			continue // always keep the serving image; doesn't use a slot
		}
		if kept < keep {
			kept++
			continue
		}
		if err := ir.ImageRemove(ctx, t, false); err != nil {
			log("retain: remove image", "image", t, "err", err)
		}
	}
}

// imageRepoPrefix derives the retention scope from the deployed image reference:
// "sigmahub/<res>:<sha>" → "sigmahub/<res>:", and a Compose service's
// "sigmahub/<res>-<svc>:<sha>" → "sigmahub/<res>-<svc>:" — so each service's
// image line is retained/pruned independently. Returns "" (retention no-op) for a
// ref without a tag or one not under the sigmahub/ namespace (a prebuilt Compose
// service image like postgres:16 is never pruned by us).
func imageRepoPrefix(image string) string {
	if !strings.HasPrefix(image, "sigmahub/") {
		return ""
	}
	i := strings.LastIndex(image, ":")
	if i <= 0 {
		return ""
	}
	return image[:i+1]
}

// inUseImages returns the image tags a resource's running managed containers use,
// plus the just-deployed tag — the set retention must never remove.
func inUseImages(ctx context.Context, dk interface {
	ContainerList(ctx context.Context) ([]ContainerState, error)
}, resourceID, deployedTag string) map[string]bool {
	inUse := map[string]bool{}
	if deployedTag != "" {
		inUse[deployedTag] = true
	}
	list, err := dk.ContainerList(ctx)
	if err != nil {
		return inUse
	}
	for _, c := range list {
		if c.Labels[LabelResourceID] == resourceID && c.Image != "" {
			inUse[c.Image] = true
		}
	}
	return inUse
}

// performRollout is the blue-green swap, factored from the Docker specifics for
// testing. body is the create body for the new generation; postStart runs after
// the new container starts (secret seeding) and aborts the rollout on error.
//
// Invariant (never cut): the new container is created, started, and HEALTH-GATED
// before any old generation is touched. A health-gate failure removes ONLY the
// new container, leaving the previous version serving.
func performRollout(ctx context.Context, dk rolloutDocker, probe Prober, spec RolloutSpec, body any, hash string, postStart func(ctx context.Context, id string) error, log logf) error {
	resourceID := spec.Container.ResourceID
	service := spec.Container.Service
	newName := rolloutName(spec.Container.Name, spec.Generation)

	// Idempotency: an unchanged new generation already running is converged —
	// still drain any stragglers, then done.
	if cur, ok, err := dk.ContainerInspect(ctx, newName); err != nil {
		return err
	} else if ok {
		if cur.Running && cur.Labels[LabelSpecHash] == hash {
			return drainOld(ctx, dk, resourceID, service, newName, log)
		}
		if cur.Running {
			// A live container already holds this generation's name but with a
			// different spec. Generation names are per-deployment, so this should
			// not occur; refuse rather than force-remove a SERVING container before
			// its replacement is health-gated (that would be a hard cut).
			return fmt.Errorf("generation %q already held by a running container with a different spec — refusing to cut", newName)
		}
		// A stopped leftover of this generation (e.g. a crashed prior attempt):
		// safe to remove before recreating — it is not serving traffic.
		if err := dk.ContainerRemove(ctx, cur.ID, true); err != nil {
			return fmt.Errorf("remove stale generation: %w", err)
		}
	}

	// 1. Create + start the NEW generation ALONGSIDE the old.
	id, err := dk.ContainerCreate(ctx, newName, body)
	if err != nil {
		return fmt.Errorf("create new generation: %w", err)
	}
	if err := dk.ContainerStart(ctx, id); err != nil {
		_ = dk.ContainerRemove(ctx, id, true)
		return fmt.Errorf("start new generation: %w", err)
	}
	if postStart != nil {
		if err := postStart(ctx, id); err != nil {
			_ = dk.ContainerRemove(ctx, id, true)
			return fmt.Errorf("post-start: %w", err)
		}
	}

	// 2. Health-gate. On failure, remove ONLY the new container — the old keeps
	// serving (never cut).
	if err := gateHealthy(ctx, dk, probe, newName, spec.Health); err != nil {
		_ = dk.ContainerRemove(ctx, id, true)
		return fmt.Errorf("new version unhealthy, kept previous: %w", err)
	}

	// 3. New is healthy and in the Traefik LB → drain the old generation(s).
	return drainOld(ctx, dk, resourceID, service, newName, log)
}

// performRecreate is the recreate swap for a Compose service holding an exclusive
// resource (a named volume or a fixed host port): the old generation(s) are
// removed FIRST, then the new one is created and started. There is a brief
// downtime window by design — two generations cannot coexist. Factored from the
// Docker specifics for testing.
func performRecreate(ctx context.Context, dk rolloutDocker, probe Prober, spec RecreateSpec, body any, hash string, postStart func(ctx context.Context, id string) error, log logf) error {
	resourceID := spec.Container.ResourceID
	service := spec.Container.Service
	newName := rolloutName(spec.Container.Name, spec.Generation)

	// Idempotency: the target generation already running and unchanged is converged.
	if cur, ok, err := dk.ContainerInspect(ctx, newName); err != nil {
		return err
	} else if ok && cur.Running && cur.Labels[LabelSpecHash] == hash {
		return drainOld(ctx, dk, resourceID, service, newName, log)
	}

	// Remove EVERY generation of this (resource, service) first — the exclusive
	// resource can't be held twice. keepName is empty so nothing is spared.
	if err := drainOld(ctx, dk, resourceID, service, "", log); err != nil {
		return fmt.Errorf("drain before recreate: %w", err)
	}

	id, err := dk.ContainerCreate(ctx, newName, body)
	if err != nil {
		return fmt.Errorf("create recreate generation: %w", err)
	}
	if err := dk.ContainerStart(ctx, id); err != nil {
		_ = dk.ContainerRemove(ctx, id, true)
		return fmt.Errorf("start recreate generation: %w", err)
	}
	if postStart != nil {
		if err := postStart(ctx, id); err != nil {
			_ = dk.ContainerRemove(ctx, id, true)
			return fmt.Errorf("post-start: %w", err)
		}
	}
	// Verify the new generation becomes healthy; on failure report it (there is no
	// old generation to fall back to — the recreate window is the documented risk).
	if err := gateHealthy(ctx, dk, probe, newName, spec.Health); err != nil {
		return fmt.Errorf("recreated service unhealthy: %w", err)
	}
	return nil
}

// opRollout handles a deploy.rollout op: a zero-downtime blue-green swap of a
// resource's container to a new generation.
func (d *Driver) opRollout(ctx context.Context, op dsd.Op) error {
	if err := d.throttle(); err != nil {
		return err
	}
	var spec RolloutSpec
	if err := json.Unmarshal(op.Spec, &spec); err != nil {
		return fmt.Errorf("decode rollout spec: %w", err)
	}
	// Deny-by-default policy gate — same as container.apply, refused regardless
	// of the (signed) DSD.
	if err := CheckPolicy(spec.Container); err != nil {
		return err
	}

	effective, fileSecrets, err := d.resolveSecrets(ctx, spec.Container)
	if err != nil {
		return err
	}
	hash := spec.Container.SpecHash()
	body := d.buildCreateBody(effective, hash)

	postStart := func(ctx context.Context, id string) error {
		if len(fileSecrets) == 0 {
			return nil
		}
		cur, ok, err := d.docker.ContainerInspect(ctx, id)
		if err != nil {
			return err
		}
		if !ok || !cur.Running || cur.Pid <= 0 {
			return fmt.Errorf("container not running after start; refusing to seed secrets to disk")
		}
		return writeFileSecrets(cur.Pid, fileSecrets)
	}

	if err := performRollout(ctx, d.docker, defaultProbe, spec, body, hash, postStart, d.log.Warn); err != nil {
		return err
	}
	// Keep the last-N built images so a rebuild-free rollback always has a target;
	// prune older ones. Best-effort — never fails the deploy.
	retainImages(ctx, d.docker, imageRepoPrefix(spec.Container.Image), defaultImageRetention,
		inUseImages(ctx, d.docker, spec.Container.ResourceID, spec.Container.Image), d.log.Warn)
	return nil
}

// opRecreate handles a deploy.recreate op: the recreate swap for a Compose service
// that holds an exclusive resource and cannot run two generations at once.
func (d *Driver) opRecreate(ctx context.Context, op dsd.Op) error {
	if err := d.throttle(); err != nil {
		return err
	}
	var spec RecreateSpec
	if err := json.Unmarshal(op.Spec, &spec); err != nil {
		return fmt.Errorf("decode recreate spec: %w", err)
	}
	if err := CheckPolicy(spec.Container); err != nil {
		return err
	}
	effective, fileSecrets, err := d.resolveSecrets(ctx, spec.Container)
	if err != nil {
		return err
	}
	hash := spec.Container.SpecHash()
	body := d.buildCreateBody(effective, hash)

	postStart := func(ctx context.Context, id string) error {
		if len(fileSecrets) == 0 {
			return nil
		}
		cur, ok, err := d.docker.ContainerInspect(ctx, id)
		if err != nil {
			return err
		}
		if !ok || !cur.Running || cur.Pid <= 0 {
			return fmt.Errorf("container not running after start; refusing to seed secrets to disk")
		}
		return writeFileSecrets(cur.Pid, fileSecrets)
	}

	if err := performRecreate(ctx, d.docker, defaultProbe, spec, body, hash, postStart, d.log.Warn); err != nil {
		return err
	}
	retainImages(ctx, d.docker, imageRepoPrefix(spec.Container.Image), defaultImageRetention,
		inUseImages(ctx, d.docker, spec.Container.ResourceID, spec.Container.Image), d.log.Warn)
	return nil
}

// resolveSecrets resolves a container's secret references into an effective spec
// (env-mode values folded in) plus the file-mode secrets to seed post-start. It
// mirrors converge's resolution so the rollout path applies secrets identically.
func (d *Driver) resolveSecrets(ctx context.Context, spec ContainerSpec) (ContainerSpec, []Secret, error) {
	effective := spec
	var fileSecrets []Secret
	if len(spec.SecretRefs) == 0 || d.secrets == nil {
		return effective, nil, nil
	}
	fetched, err := d.secrets(ctx, spec.ResourceID)
	if err != nil {
		return spec, nil, fmt.Errorf("fetch secrets: %w", err)
	}
	byName := make(map[string]Secret, len(fetched))
	for _, s := range fetched {
		byName[s.Name] = s
	}
	effective.Env = map[string]string{}
	for k, v := range spec.Env {
		effective.Env[k] = v
	}
	for _, ref := range spec.SecretRefs {
		s, ok := byName[ref.Name]
		if !ok {
			return spec, nil, fmt.Errorf("secret %q referenced but not provided", ref.Name)
		}
		if ref.EnvVar {
			effective.Env[ref.Name] = s.Value
		} else {
			fileSecrets = append(fileSecrets, s)
		}
	}
	return effective, fileSecrets, nil
}
