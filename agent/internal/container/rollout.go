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

// olderGenerations returns the managed containers belonging to the resource whose
// name is not keepName — the previous generations to drain after the new one is
// healthy.
func olderGenerations(list []ContainerState, resourceID, keepName string) []ContainerState {
	var out []ContainerState
	for _, c := range list {
		if c.Labels[LabelResourceID] == resourceID && c.Name != keepName {
			out = append(out, c)
		}
	}
	return out
}

// defaultProbe is the real health probe: an HTTP GET expecting a 2xx, or a bare
// TCP dial. A single attempt — gateHealthy retries it until the deadline.
func defaultProbe(ctx context.Context, ip string, p HealthProbe) error {
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
// resource. A graceful stop lets Traefik drop the backend and in-flight requests
// complete before the container goes away.
func drainOld(ctx context.Context, dk rolloutDocker, resourceID, keepName string, log logf) error {
	list, err := dk.ContainerList(ctx)
	if err != nil {
		return err
	}
	for _, c := range olderGenerations(list, resourceID, keepName) {
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

// retainImages keeps the newest `keep` images tagged sigmahub/<resourceID>:* and
// removes older tags, never removing one in `inUse` (a running generation — the
// image currently serving must survive). An in-use tag does not consume a keep
// slot, so `keep` historical images are retained BEYOND the running one.
// Best-effort: a removal failure is logged, never fatal — a deploy must not fail
// because an old image couldn't be pruned.
func retainImages(ctx context.Context, ir imageRetainer, resourceID string, keep int, inUse map[string]bool, log logf) {
	if keep <= 0 {
		keep = defaultImageRetention
	}
	prefix := imageTagPrefix(resourceID)
	imgs, err := ir.ImageList(ctx, prefix+"*")
	if err != nil {
		log("retain: list images", "resource", resourceID, "err", err)
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

// imageTagPrefix is the "sigmahub/<resourceID>:" tag prefix — mirrors the CP's
// dsd.DeployImageTag so retention scopes to exactly one resource's images.
func imageTagPrefix(resourceID string) string {
	return "sigmahub/" + resourceID + ":"
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
	newName := rolloutName(spec.Container.Name, spec.Generation)

	// Idempotency: an unchanged new generation already running is converged —
	// still drain any stragglers, then done.
	if cur, ok, err := dk.ContainerInspect(ctx, newName); err != nil {
		return err
	} else if ok {
		if cur.Running && cur.Labels[LabelSpecHash] == hash {
			return drainOld(ctx, dk, resourceID, newName, log)
		}
		// A stale/leftover same-name container: remove before recreating.
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
	return drainOld(ctx, dk, resourceID, newName, log)
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
	retainImages(ctx, d.docker, spec.Container.ResourceID, defaultImageRetention,
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
