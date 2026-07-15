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
	KindNetworkEnsure = "network.ensure"
	KindVolumeEnsure  = "volume.ensure"
	KindImagePull     = "image.pull"
	KindContainerApply = "container.apply"
	KindVolumeRemove  = "volume.remove"
)

// Managed-object labels. Every container/volume/network the agent creates
// carries these so the reconcile loop can enumerate and garbage-collect them
// without a control-plane round-trip.
const (
	LabelManaged    = "sigmahub.managed"    // "true"
	LabelResourceID = "sigmahub.resourceId" // owning resource id
	LabelSpecHash   = "sigmahub.specHash"   // hash of the applied container config
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
// publish" (the port is only reachable on the project network).
type PortMapping struct {
	Container int    `json:"container"`
	Host      int    `json:"host,omitempty"`
	Protocol  string `json:"protocol,omitempty"` // tcp (default) | udp
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
	ResourceID     string            `json:"resourceId"`
	Name           string            `json:"name"`
	Image          string            `json:"image"`
	Network        string            `json:"network"`
	Env            map[string]string `json:"env,omitempty"`
	Ports          []PortMapping     `json:"ports,omitempty"`
	Volumes        []VolumeMount     `json:"volumes,omitempty"`
	Command        []string          `json:"command,omitempty"`
	User           string            `json:"user,omitempty"`
	ReadOnlyRootfs bool              `json:"readOnlyRootfs,omitempty"`
	// Tmpfs mount points (e.g. /tmp, /var/cache/nginx). Required to run a
	// read-only-rootfs image that still needs scratch space.
	Tmpfs          []string          `json:"tmpfs,omitempty"`
	CPUs           float64           `json:"cpus,omitempty"`
	MemoryMB       int64             `json:"memoryMb,omitempty"`
	Restart        string            `json:"restart,omitempty"` // no|on-failure|always|unless-stopped
	// SecretRefs are references (never values) to secrets injected at
	// container-create; the agent resolves them via the control plane.
	SecretRefs     []SecretRef       `json:"secretRefs,omitempty"`

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
