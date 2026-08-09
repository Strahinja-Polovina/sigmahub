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
	"github.com/Strahinja-Polovina/sigmahub/agent/internal/build"
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
	// startup ships a failed generation's own output to the deploy log. Nil
	// leaves the behaviour as it was: the container is removed and its account
	// of why it never started goes with it.
	startup startupSink
	// registry resolves the org's registry credential for a pull of an image
	// this fleet built and pushed (SIGMA-243). Nil on a host that only ever
	// pulls public images — and, before this existed, on every host, which is
	// why the dedicated build server could not be used with a private registry.
	registry RegistryFetcher
	// serverID stamps every object this agent creates with its owner, so GC can
	// leave a peer's containers alone. Empty until registration completes.
	serverID string
	// mu serialises the reconcile loop's per-container converge against GC's
	// prune+remove, so a container GC just removed cannot be resurrected by a
	// concurrent reconcile working from a stale desired snapshot.
	mu sync.Mutex
}

// NewDriver builds a driver. The allowlist ships disabled (see allowlist.go).
// fetcher may be nil to disable secret injection.
// SetServerID tells the driver which server it is, so everything it creates
// carries an owner and GC can tell its own objects from a peer's. Set after
// registration, because the id does not exist before it.
func (d *Driver) SetServerID(id string) { d.serverID = id }

func NewDriver(docker *DockerClient, store *Store, log *slog.Logger, fetcher SecretFetcher) *Driver {
	return &Driver{
		docker:  docker,
		store:   store,
		log:     log,
		limiter: newRateLimiter(20, 5), // burst 20, 5 ops/sec sustained
		secrets: fetcher,
	}
}

// SetStartupLogSink installs the channel a failed generation's output is shipped
// on. Separate from NewDriver because the control-plane client is built after
// the driver, and a host with no sink still deploys — it just cannot explain a
// crash-on-boot.
func (d *Driver) SetStartupLogSink(sink func(ctx context.Context, deploymentID string, lines []string)) {
	d.startup = sink
}

// RegistryFetcher resolves the org's registry credential from the control
// plane. It is the same shape the build path's push already uses — the two
// halves of one pipeline authenticate against the same registry with the same
// credential, and only the push half ever did (SIGMA-243).
type RegistryFetcher func(ctx context.Context) (build.RegistryAuth, error)

// SetRegistryFetcher installs the credential source for pulls of images this
// fleet built. Separate from NewDriver for the same reason as the startup sink:
// the control-plane client is built after the driver, and a host with no fetcher
// still deploys — it just cannot pull from a private registry.
func (d *Driver) SetRegistryFetcher(f RegistryFetcher) { d.registry = f }

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
	r.Register(KindDeployRollout, d.opRollout)
	r.Register(KindDeployRecreate, d.opRecreate)
}

func (d *Driver) throttle() error {
	if !d.limiter.allow() {
		return fmt.Errorf("rate limited: too many container ops; will retry on next resync")
	}
	return nil
}

func (d *Driver) managedLabels(resourceID string) map[string]string {
	l := map[string]string{
		LabelManaged:    "true",
		LabelResourceID: resourceID,
	}
	if d.serverID != "" {
		l[LabelServerID] = d.serverID
	}
	return l
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
	return d.docker.VolumeCreate(ctx, spec.Name, d.managedLabels(spec.ResourceID))
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
	// An image that lives in the org's own registry needs the org's credential:
	// this is an image OUR build server pushed, and a private registry — the
	// default for GHCR packages — refuses an anonymous pull (SIGMA-243). Fail
	// here, naming the registry, rather than pulling anonymously and letting the
	// deploy die on an opaque "denied" from the daemon.
	var auth build.RegistryAuth
	if spec.RegistryHost != "" {
		if d.registry == nil {
			return fmt.Errorf("pull from %s needs a registry credential and this agent has no way to fetch one", spec.RegistryHost)
		}
		got, err := d.registry(ctx)
		if err != nil {
			return fmt.Errorf("resolve registry credential for %s: %w", spec.RegistryHost, err)
		}
		auth = got
		if auth.Host == "" {
			auth.Host = spec.RegistryHost
		}
	}
	return d.docker.ImagePullAuth(ctx, spec.Image, auth)
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

// persistRolloutGeneration records a just-deployed rollout/recreate generation
// as desired so the reconcile loop can repair it after an out-of-band stop or
// removal (SIGMA-146), and drops the desired records of older generations of the
// same (resource, service) so a drained generation is never resurrected. The
// stored spec's Name is the generation-suffixed container name and its SpecHash
// matches the label stamped at create time, so a healthy running generation is
// seen as converged rather than needlessly recreated.
func (d *Driver) persistRolloutGeneration(genSpec ContainerSpec) error {
	if all, err := d.store.AllDesired(); err != nil {
		d.log.Warn("rollout: read desired for prune", "err", err)
	} else {
		for name, s := range all {
			if name != genSpec.Name && s.ResourceID == genSpec.ResourceID && s.Service == genSpec.Service {
				if err := d.store.DeleteDesired(name); err != nil {
					d.log.Warn("rollout: prune old generation desired", "container", name, "err", err)
				}
			}
		}
	}
	return d.store.PutDesired(genSpec.Name, genSpec)
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
	for k, v := range d.managedLabels(spec.ResourceID) {
		labels[k] = v
	}
	labels[LabelSpecHash] = specHash
	if spec.Service != "" {
		labels[LabelService] = spec.Service
	}

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
			binding := map[string]string{"HostPort": fmt.Sprintf("%d", p.Host)}
			// A HostIP-restricted binding (P1-10 mesh-only databases) publishes on
			// exactly that address; without it Docker binds 0.0.0.0.
			if p.HostIP != "" {
				binding["HostIp"] = p.HostIP
			}
			portBindings[key] = []map[string]string{binding}
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
	if spec.ShmSizeMB > 0 {
		hostConfig["ShmSize"] = spec.ShmSizeMB * 1024 * 1024
	}
	// GPU passthrough via the NVIDIA container runtime. Count -1 is Docker's
	// "every device"; a positive count requests exactly that many. Capability
	// "gpu" is what the runtime hook matches on.
	if spec.GPUs != 0 {
		count := spec.GPUs
		if count < 0 {
			count = -1
		}
		hostConfig["DeviceRequests"] = []map[string]any{{
			"Driver":       "nvidia",
			"Count":        count,
			"Capabilities": [][]string{{"gpu"}},
		}}
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
	// Compose service discovery: publish the service name as a network alias so
	// sibling services resolve each other by name on the shared project network.
	if len(spec.NetworkAliases) > 0 && spec.Network != "" {
		body["NetworkingConfig"] = map[string]any{
			"EndpointsConfig": map[string]any{
				spec.Network: map[string]any{"Aliases": spec.NetworkAliases},
			},
		}
	}
	if len(spec.Command) > 0 {
		body["Cmd"] = spec.Command
	}
	if spec.User != "" {
		body["User"] = spec.User
	}
	return body
}
