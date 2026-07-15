package container

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
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
}

// NewDriver builds a driver. The allowlist ships disabled (see allowlist.go).
func NewDriver(docker *DockerClient, store *Store, log *slog.Logger) *Driver {
	return &Driver{
		docker:  docker,
		store:   store,
		log:     log,
		limiter: newRateLimiter(20, 5), // burst 20, 5 ops/sec sustained
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

// converge inspects the named container and (re)creates it if it is absent,
// stopped, or its spec-hash label no longer matches the desired spec.
func (d *Driver) converge(ctx context.Context, spec ContainerSpec) error {
	want := spec.SpecHash()
	cur, exists, err := d.docker.ContainerInspect(ctx, spec.Name)
	if err != nil {
		return err
	}
	if exists && cur.Running && cur.Labels[LabelSpecHash] == want {
		return nil // already converged
	}
	if exists {
		if err := d.docker.ContainerRemove(ctx, cur.ID, true); err != nil {
			return fmt.Errorf("remove stale container: %w", err)
		}
	}
	body := d.buildCreateBody(spec, want)
	id, err := d.docker.ContainerCreate(ctx, spec.Name, body)
	if err != nil {
		return fmt.Errorf("create container: %w", err)
	}
	if err := d.docker.ContainerStart(ctx, id); err != nil {
		return fmt.Errorf("start container: %w", err)
	}
	return nil
}

func (d *Driver) opVolumeRemove(ctx context.Context, op dsd.Op) error {
	if err := d.throttle(); err != nil {
		return err
	}
	var spec VolumeSpec
	if err := json.Unmarshal(op.Spec, &spec); err != nil {
		return fmt.Errorf("decode volume spec: %w", err)
	}
	return d.docker.VolumeRemove(ctx, spec.Name, true)
}

// buildCreateBody assembles the Docker /containers/create body, applying the
// UNCONDITIONAL isolation defaults (no-new-privileges, docker-default AppArmor,
// the daemon's default seccomp profile via not disabling it, cgroup v2 CPU/mem
// limits, per-project network) plus the SPEC-DRIVEN ones (non-root user,
// read-only rootfs). specHash is stamped as a label for drift detection.
func (d *Driver) buildCreateBody(spec ContainerSpec, specHash string) map[string]any {
	labels := managedLabels(spec.ResourceID)
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
