// Package container is sigmad's Docker runtime driver. It registers the typed
// container op kinds behind the P1-2 apply registry (network.ensure,
// volume.ensure, image.pull, container.apply, volume.remove), enforces a local
// deny-by-default policy independent of DSD contents, applies unconditional
// isolation defaults to every container, and runs an actual-state reconcile
// loop that keeps managed workloads converged even while the control plane is
// unreachable.
//
// The Docker Engine is driven over its REST API on the local unix socket with
// the standard library only — no heavyweight SDK — so the agent stays a small,
// auditable binary.
package container

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Op kinds registered on both the control plane (reconciler render) and the
// agent (apply registry). Kept in one place so the two sides cannot drift.
const (
	KindNetworkEnsure  = "network.ensure"
	KindVolumeEnsure   = "volume.ensure"
	KindImagePull      = "image.pull"
	KindContainerApply = "container.apply"
	KindVolumeRemove   = "volume.remove"
	// KindDeployRollout is the P1-9 zero-downtime blue-green rollout: a new
	// generation of a resource's container is started alongside the old, health-
	// gated, and only after it passes is the old drained. Name matches the CP.
	KindDeployRollout = "deploy.rollout"
	// KindDeployRecreate is the P1-9 recreate swap for a Compose service that holds
	// an exclusive resource (a named volume or a fixed host port) and so cannot run
	// two generations at once: the old generation is removed, THEN the new one is
	// created — a documented, per-service exception to the zero-downtime guarantee.
	KindDeployRecreate = "deploy.recreate"
)

// HealthProbe is how the agent decides a new container is ready before draining
// the old (the never-cut invariant). Type "http" GETs Path on Port expecting a
// 2xx; "tcp" just dials Port. Derived from the resource's health spec (P1-7/P1-8).
type HealthProbe struct {
	Type        string `json:"type"` // http | tcp
	Path        string `json:"path,omitempty"`
	Port        int    `json:"port,omitempty"`
	IntervalSec int    `json:"intervalSec,omitempty"`
	TimeoutSec  int    `json:"timeoutSec,omitempty"` // overall gate deadline
}

// RolloutSpec is the payload of a deploy.rollout op: the desired container plus
// the generation tag that names its instance and the health probe that gates the
// swap.
type RolloutSpec struct {
	Container    ContainerSpec `json:"container"`
	Generation   string        `json:"generation"` // e.g. the 8-char git SHA
	Health       HealthProbe   `json:"health"`
	DeploymentID string        `json:"deploymentId,omitempty"`
}

// RecreateSpec is the payload of a deploy.recreate op: the old generation of the
// (resource, service) is removed before the new one is created, so a service
// holding an exclusive resource never has two live generations. There is a brief
// downtime window by design — the documented per-service exception.
type RecreateSpec struct {
	Container    ContainerSpec `json:"container"`
	Generation   string        `json:"generation"`
	Health       HealthProbe   `json:"health"`
	DeploymentID string        `json:"deploymentId,omitempty"`
}

// Managed-object labels. Every container/volume/network the agent creates
// carries these so the reconcile loop can enumerate and garbage-collect them
// without a control-plane round-trip.
const (
	LabelManaged    = "sigmahub.managed"    // "true"
	LabelResourceID = "sigmahub.resourceId" // owning resource id
	LabelSpecHash   = "sigmahub.specHash"   // hash of the applied container config
	// LabelService names the Compose service a container belongs to, so a
	// multi-service resource's rollout/recreate and GC scope to one service's
	// generations. Empty (label absent) for a single-container app.
	LabelService = "sigmahub.service"
	// LabelServerID names the agent that created the object.
	//
	// On a real host there is one agent, so this is redundant — which is exactly
	// why it went unstamped and why GC could reap anything wearing the managed
	// label. The fleet e2e runs several agents against ONE Docker daemon, and
	// there each agent's GC saw its peers' containers as orphans and removed
	// them: the placed service came up, the other host's reconcile deleted it,
	// and a multi-server deploy could never converge. Stamping the owner makes
	// "not mine" expressible; an object with no owner label is still reaped,
	// since on a real host it can only be this agent's own from an older build.
	LabelServerID = "sigmahub.serverId"
)

// NetworkSpec is the payload of a network.ensure op.
type NetworkSpec struct {
	Name string `json:"name"`
}

// VolumeSpec is the payload of a volume.ensure / volume.remove op.
type VolumeSpec struct {
	Name       string `json:"name"`
	ResourceID string `json:"resourceId,omitempty"`
}

// ImageSpec is the payload of an image.pull op.
type ImageSpec struct {
	Image string `json:"image"`
}

// PortMapping publishes a container port on the host. Host 0 means "do not
// publish" (the port is only reachable on the project network). HostIP, when
// set, restricts the published port to that one host address — P1-10 databases
// bind to the server's mesh IP so the engine is never publicly reachable.
type PortMapping struct {
	Container int    `json:"container"`
	Host      int    `json:"host,omitempty"`
	Protocol  string `json:"protocol,omitempty"` // tcp (default) | udp
	HostIP    string `json:"hostIp,omitempty"`
}

// VolumeMount attaches a named volume. Host-path binds are intentionally not
// representable here — they are only expressible via HostMounts, which the
// policy always rejects.
type VolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

// HostMount is a raw host-path bind. The control plane never emits these; the
// field exists solely so the agent policy can detect and reject a DSD that
// tries to smuggle one in.
type HostMount struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

// SecretsMountDir is the in-memory (tmpfs) directory file-mode secrets are
// seeded into. Kept in sync with the control plane's secretsMountDir.
const SecretsMountDir = "/run/secrets"

// SecretRef references a secret the agent fetches and injects at
// container-create — never the value. EnvVar false = a tmpfs file under
// SecretsMountDir; true = an environment variable (explicit opt-in).
type SecretRef struct {
	Name   string `json:"name"`
	EnvVar bool   `json:"envVar,omitempty"`
}

// Secret is a resolved secret value fetched from the control plane for
// injection. It never enters the persisted desired-state (only references do).
type Secret struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	EnvVar bool   `json:"envVar"`
}

// ContainerSpec is the payload of a container.apply op: the full desired state
// of one workload container. The control plane renders it from a resource's
// spec; the agent applies it after the local policy gate passes.
type ContainerSpec struct {
	ResourceID string `json:"resourceId"`
	// Service is the Compose service name for a multi-service resource (empty for a
	// single-container app). It scopes the container's rollout/recreate/GC group
	// and becomes a network alias so sibling services reach it by service name.
	Service string `json:"service,omitempty"`
	Name    string `json:"name"`
	Image   string `json:"image"`
	Network string `json:"network"`
	// NetworkAliases are extra DNS aliases the container answers to on its network
	// (the Compose service name, so `db`, `web`, … resolve between services).
	NetworkAliases []string          `json:"networkAliases,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	Ports          []PortMapping     `json:"ports,omitempty"`
	Volumes        []VolumeMount     `json:"volumes,omitempty"`
	Command        []string          `json:"command,omitempty"`
	User           string            `json:"user,omitempty"`
	ReadOnlyRootfs bool              `json:"readOnlyRootfs,omitempty"`
	// Tmpfs mount points (e.g. /tmp, /var/cache/nginx). Required to run a
	// read-only-rootfs image that still needs scratch space.
	Tmpfs    []string `json:"tmpfs,omitempty"`
	CPUs     float64  `json:"cpus,omitempty"`
	MemoryMB int64    `json:"memoryMb,omitempty"`
	Restart  string   `json:"restart,omitempty"` // no|on-failure|always|unless-stopped
	// SecretRefs are references (never values) to secrets injected at
	// container-create; the agent resolves them via the control plane.
	SecretRefs []SecretRef `json:"secretRefs,omitempty"`
	// Labels are extra Docker labels merged onto the container — the P1-8 Traefik
	// router labels ride here. Part of the spec hash, so a label change (a domain
	// attached/detached) triggers a recreate. The sigmahub.* managed labels are
	// applied separately and win on any key collision.
	Labels map[string]string `json:"labels,omitempty"`

	// GPUs requests NVIDIA devices (-1 = every GPU on the host). Without it an
	// inference runtime silently falls back to CPU. Part of the spec hash, so
	// changing the request recreates the container.
	GPUs int `json:"gpus,omitempty"`
	// ShmSizeMB overrides Docker's 64 MB default /dev/shm. Model loading needs a
	// large shared-memory segment; the default fails with an unhelpful error.
	ShmSizeMB int64 `json:"shmSizeMb,omitempty"`
	// Forbidden by the local policy. The control plane never sets these; they
	// exist only so the agent can detect and reject a DSD that does.
	Privileged  bool        `json:"privileged,omitempty"`
	HostNetwork bool        `json:"hostNetwork,omitempty"`
	HostPID     bool        `json:"hostPid,omitempty"`
	HostMounts  []HostMount `json:"hostMounts,omitempty"`
}

// SpecHash is a stable fingerprint of a container's desired configuration. The
// agent stores it as a label and compares it on every reconcile: a changed
// hash means recreate, an unchanged hash means the running container already
// matches (idempotent apply, cheap drift check). json.Marshal of a struct is
// deterministic (fixed field order), so the hash is reproducible.
func (s ContainerSpec) SpecHash() string {
	b, _ := json.Marshal(s)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}
