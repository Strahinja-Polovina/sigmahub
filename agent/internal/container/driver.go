package container

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/agent/internal/apply"
	"github.com/Strahinja-Polovina/sigmahub/agent/internal/dsd"
)

// stopGrace is how long Docker waits for a container to exit on stop/recreate.
const stopGrace = 10 * time.Second

// Driver owns the Docker client and the desired-state store and implements the
// container op handlers registered behind the P1-2 apply registry.
type Driver struct {
	docker    *DockerClient
	store     *Store
	log       *slog.Logger
	limiter   *rateLimiter
	allowlist ImageAllowlist
	// secrets resolves a resource's secret values from the control plane at
	// container-create; nil disables secret injection (e.g. in tests).
	secrets SecretFetcher
	// mu serialises the reconcile loop's per-container converge against GC's
	// prune+remove, so a container GC just removed cannot be resurrected by a
	// concurrent reconcile working from a stale desired snapshot.
	mu sync.Mutex
}

// NewDriver builds a driver. The allowlist ships disabled (see allowlist.go).
// fetcher may be nil to disable secret injection.
func NewDriver(docker *DockerClient, store *Store, log *slog.Logger, fetcher SecretFetcher) *Driver {
	return &Driver{
		docker:  docker,
		store:   store,
		log:     log,
		limiter: newRateLimiter(20, 5), // burst 20, 5 ops/sec sustained
		secrets: fetcher,
	}
}

// Register wires every container op kind into the apply registry. This is the
// single place these capabilities come into existence — an op kind not
// registered here is rejected by Apply as "unknown op kind", preserving the
// no-generic-run-shell invariant.
func (d *Driver) Register(r *apply.Registry) {
	r.Register(KindNetworkEnsure, d.opNetworkEnsure)
	r.Register(KindVolumeEnsure, d.opVolumeEnsure)
	r.Register(KindImagePull, d.opImagePull)
	r.Register(KindContainerApply, d.opContainerApply)
	r.Register(KindVolumeRemove, d.opVolumeRemove)
	r.Register(KindProxyTraefik, d.opProxyTraefik)
}

func (d *Driver) throttle() error {
	if !d.limiter.allow() {
		return fmt.Errorf("rate limited: too many container ops; will retry on next resync")
	}
	return nil
}

func managedLabels(resourceID string) map[string]string {
	return map[string]string{
		LabelManaged:    "true",
		LabelResourceID: resourceID,
	}
}

func (d *Driver) opNetworkEnsure(ctx context.Context, op dsd.Op) error {
	if err := d.throttle(); err != nil {
		return err
	}
	var spec NetworkSpec
	if err := json.Unmarshal(op.Spec, &spec); err != nil {
		return fmt.Errorf("decode network spec: %w", err)
	}
	exists, err := d.docker.NetworkExists(ctx, spec.Name)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	return d.docker.NetworkCreate(ctx, spec.Name, map[string]string{LabelManaged: "true"})
}

func (d *Driver) opVolumeEnsure(ctx context.Context, op dsd.Op) error {
	if err := d.throttle(); err != nil {
		return err
	}
	var spec VolumeSpec
	if err := json.Unmarshal(op.Spec, &spec); err != nil {
		return fmt.Errorf("decode volume spec: %w", err)
	}
	exists, err := d.docker.VolumeExists(ctx, spec.Name)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	return d.docker.VolumeCreate(ctx, spec.Name, managedLabels(spec.ResourceID))
}

func (d *Driver) opImagePull(ctx context.Context, op dsd.Op) error {
	if err := d.throttle(); err != nil {
		return err
	}
	var spec ImageSpec
	if err := json.Unmarshal(op.Spec, &spec); err != nil {
		return fmt.Errorf("decode image spec: %w", err)
	}
	if err := d.allowlist.Check(spec.Image); err != nil {
		return err
	}
	return d.docker.ImagePull(ctx, spec.Image)
}

// opContainerApply converges one container to its desired spec. It is
// idempotent: an existing container carrying the same spec-hash label and
// already running is left untouched; otherwise it is (re)created. The desired
// spec is persisted so the reconcile loop can re-converge drift offline.
func (d *Driver) opContainerApply(ctx context.Context, op dsd.Op) error {
	if err := d.throttle(); err != nil {
		return err
	}
	var spec ContainerSpec
	if err := json.Unmarshal(op.Spec, &spec); err != nil {
		return fmt.Errorf("decode container spec: %w", err)
	}
	// Deny-by-default policy gate — refused regardless of the (signed) DSD.
	if err := CheckPolicy(spec); err != nil {
		return err
	}
	if err := d.converge(ctx, spec); err != nil {
		return err
	}
	// Record desired state only after a successful converge.
	return d.store.PutDesired(spec.Name, spec)
}

// converged reports whether an existing container already satisfies the spec.
// A running container with a matching spec-hash is converged. A STOPPED
// container with a matching hash is also converged when the spec's restart
// policy is terminal ("no"/"on-failure" — the operator expects it may stay
// stopped); under a keep-running policy a stopped container is genuine drift to
// repair. This stops the reconcile loop from force-recreating a cleanly-exited
// one-shot container on every tick.
func converged(spec ContainerSpec, cur ContainerState, exists bool) bool {
	if !exists || cur.Labels[LabelSpecHash] != spec.SpecHash() {
		return false
	}
	if cur.Running {
		return true
	}
	return terminalRestart(spec.Restart)
}

func terminalRestart(policy string) bool {
	switch policy {
	case "no", "on-failure":
		return true
	default: // "" (defaults to unless-stopped), "always", "unless-stopped"
		return false
	}
}

// converge inspects the named container and (re)creates it unless it already
// satisfies the spec (see converged). Secret references are resolved from the
// control plane and injected at create: env-mode values into the container's
// environment, file-mode values seeded into an in-memory tmpfs so they never
// touch host disk. The spec hash used for the label/drift check is computed
// from the reference-only spec, so it stays stable and leaks no values.
func (d *Driver) converge(ctx context.Context, spec ContainerSpec) error {
	cur, exists, err := d.docker.ContainerInspect(ctx, spec.Name)
	if err != nil {
		return err
	}
	if converged(spec, cur, exists) {
		return nil
	}

	// Resolve secrets before create. Env-mode values go into a COPY of Env so
	// the persisted/hashed spec keeps references only; file-mode values are
	// seeded post-create into the tmpfs.
	want := spec.SpecHash()
	effective := spec
	var fileSecrets []Secret
	if len(spec.SecretRefs) > 0 && d.secrets != nil {
		fetched, err := d.secrets(ctx, spec.ResourceID)
		if err != nil {
			return fmt.Errorf("fetch secrets: %w", err)
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
				return fmt.Errorf("secret %q referenced but not provided by the control plane", ref.Name)
			}
			if ref.EnvVar {
				effective.Env[ref.Name] = s.Value
			} else {
				fileSecrets = append(fileSecrets, s)
			}
		}
	}

	if exists {
		if err := d.docker.ContainerRemove(ctx, cur.ID, true); err != nil {
			return fmt.Errorf("remove stale container: %w", err)
		}
	}
	body := d.buildCreateBody(effective, want)
	id, err := d.docker.ContainerCreate(ctx, spec.Name, body)
	if err != nil {
		return fmt.Errorf("create container: %w", err)
	}
	if err := d.docker.ContainerStart(ctx, id); err != nil {
		return fmt.Errorf("start container: %w", err)
	}
	// Seed file-mode secrets AFTER start, through the RUNNING container's mount
	// namespace (see writeFileSecrets). A tmpfs is a runtime mount that only
	// exists while the container runs, so we must re-inspect for a live pid first.
	// If the container has already exited (a crash, or a one-shot workload), we do
	// NOT seed: writing to a stopped container would land the plaintext on the
	// host disk layer. Instead we roll the container back so the next reconcile
	// retries, rather than leave a half-provisioned workload or leak to disk.
	if len(fileSecrets) > 0 {
		cur, ok, err := d.docker.ContainerInspect(ctx, id)
		if err != nil {
			d.removeQuietly(ctx, id)
			return fmt.Errorf("inspect for secret seeding: %w", err)
		}
		if !ok || !cur.Running || cur.Pid <= 0 {
			d.removeQuietly(ctx, id)
			return fmt.Errorf("container %q not running after start; refusing to seed secrets to disk", spec.Name)
		}
		if err := writeFileSecrets(cur.Pid, fileSecrets); err != nil {
			// A running container without its required secret files is a broken
			// half-provisioned state; remove it so the next reconcile recreates
			// cleanly rather than leaving a workload that can never read its config.
			d.removeQuietly(ctx, id)
			return fmt.Errorf("seed secret files: %w", err)
		}
	}
	return nil
}

// removeQuietly force-removes a container ignoring errors, used to roll back a
// partially-provisioned container so a later reconcile can recreate it.
func (d *Driver) removeQuietly(ctx context.Context, id string) {
	if err := d.docker.ContainerRemove(ctx, id, true); err != nil {
		d.log.Warn("rollback container remove failed", "id", id, "err", err)
	}
}

func (d *Driver) opVolumeRemove(ctx context.Context, op dsd.Op) error {
	if err := d.throttle(); err != nil {
		return err
	}
	var spec VolumeSpec
	if err := json.Unmarshal(op.Spec, &spec); err != nil {
		return fmt.Errorf("decode volume spec: %w", err)
	}
	// Managed guard: only ever delete a volume this agent created. Even a
	// validly-signed, confirmed volume.remove must not destroy an unmanaged or
	// foreign volume (a mistyped or malicious target), so refuse anything not
	// carrying our managed label. A missing volume is a no-op (idempotent).
	labels, exists, err := d.docker.VolumeInspect(ctx, spec.Name)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if labels[LabelManaged] != "true" {
		return &PolicyError{Rule: "volume-unmanaged", Detail: fmt.Sprintf("refusing to remove unmanaged volume %q", spec.Name)}
	}
	return d.docker.VolumeRemove(ctx, spec.Name, true)
}

// buildCreateBody assembles the Docker /containers/create body, applying the
// UNCONDITIONAL isolation defaults (no-new-privileges, docker-default AppArmor,
// the daemon's default seccomp profile via not disabling it, cgroup v2 CPU/mem
// limits, per-project network) plus the SPEC-DRIVEN ones (non-root user,
// read-only rootfs). specHash is stamped as a label for drift detection.
func (d *Driver) buildCreateBody(spec ContainerSpec, specHash string) map[string]any {
	// Caller labels first (e.g. P1-8 Traefik routers), then the managed
	// sigmahub.* labels overwrite so they always win on a key collision.
	labels := map[string]string{}
	for k, v := range spec.Labels {
		labels[k] = v
	}
	for k, v := range managedLabels(spec.ResourceID) {
		labels[k] = v
	}
	labels[LabelSpecHash] = specHash

	env := make([]string, 0, len(spec.Env))
	for k, v := range spec.Env {
		env = append(env, k+"="+v)
	}
	sort.Strings(env) // deterministic ordering

	exposed := map[string]any{}
	portBindings := map[string]any{}
	for _, p := range spec.Ports {
		proto := p.Protocol
		if proto == "" {
			proto = "tcp"
		}
		key := fmt.Sprintf("%d/%s", p.Container, proto)
		exposed[key] = map[string]any{}
		if p.Host > 0 {
			portBindings[key] = []map[string]string{{"HostPort": fmt.Sprintf("%d", p.Host)}}
		}
	}

	binds := make([]string, 0, len(spec.Volumes))
	for _, v := range spec.Volumes {
		mode := "rw"
		if v.ReadOnly {
			mode = "ro"
		}
		binds = append(binds, fmt.Sprintf("%s:%s:%s", v.Name, v.MountPath, mode))
	}
	sort.Strings(binds)

	// UNCONDITIONAL isolation. We do NOT set seccomp=unconfined, so the daemon's
	// default seccomp profile applies; apparmor is pinned to the default profile
	// explicitly so it is visible in `docker inspect`.
	securityOpt := []string{"no-new-privileges:true", "apparmor=docker-default"}

	hostConfig := map[string]any{
		"NetworkMode":    spec.Network,
		"SecurityOpt":    securityOpt,
		"Privileged":     false,
		"ReadonlyRootfs": spec.ReadOnlyRootfs,
		"Binds":          binds,
		"PortBindings":   portBindings,
	}
	if len(spec.Tmpfs) > 0 {
		tmpfs := map[string]string{}
		for _, p := range spec.Tmpfs {
			tmpfs[p] = "" // default options (rw,noexec,nosuid)
		}
		hostConfig["Tmpfs"] = tmpfs
	}
	if spec.CPUs > 0 {
		hostConfig["NanoCpus"] = int64(spec.CPUs * 1e9)
	}
	if spec.MemoryMB > 0 {
		hostConfig["Memory"] = spec.MemoryMB * 1024 * 1024
	}
	restart := spec.Restart
	if restart == "" {
		restart = "unless-stopped"
	}
	hostConfig["RestartPolicy"] = map[string]any{"Name": restart}

	body := map[string]any{
		"Image":        spec.Image,
		"Labels":       labels,
		"Env":          env,
		"ExposedPorts": exposed,
		"HostConfig":   hostConfig,
	}
	if len(spec.Command) > 0 {
		body["Cmd"] = spec.Command
	}
	if spec.User != "" {
		body["User"] = spec.User
	}
	return body
}
