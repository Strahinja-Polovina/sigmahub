package dsd

// Op kinds shared with the agent's apply registry. Defined here (a leaf package)
// so the reconciler render and the agent apply cannot drift on the wire names.
const (
	KindResourceSync   = "resource.sync"
	KindNetworkEnsure  = "network.ensure"
	KindVolumeEnsure   = "volume.ensure"
	KindImagePull      = "image.pull"
	KindContainerApply = "container.apply"
	KindVolumeRemove   = "volume.remove"
	// Host-hardening ops (P1-5) — the ONLY channel for post-enrollment host
	// changes. Names must match the agent's host package byte-for-byte.
	KindHostNftables = "host.nftables"
	KindHostSSHD     = "host.sshd"
	KindHostCIS      = "host.cis"
	// Ingress (P1-8): the Traefik proxy on a proxy-role server. Name must match
	// the agent's proxy package byte-for-byte.
	KindProxyTraefik = "proxy.traefik"
	// Git deploy pipeline (P1-9): clone at SHA → build on target → zero-downtime
	// rollout. Names must match the agent's build/container packages byte-for-byte.
	KindGitClone      = "git.clone"
	KindImageBuild    = "image.build"
	KindDeployRollout = "deploy.rollout"
)

// DeployImageTag is the deterministic local image tag for a resource at a SHA,
// so a clone/build/rollout chain and a rollback reference the same image.
func DeployImageTag(resourceID, sha string) string {
	return "sigmahub/" + resourceID + ":" + sha
}

// TraefikRouterName is the deterministic Traefik router/service name for a
// resource, so a resync renders byte-identical labels.
func TraefikRouterName(resourceID string) string { return "sigmahub-" + resourceID }

// Docker object naming. The control plane is the sole authority for these
// names; it renders them into DSD ops and the agent uses them verbatim, so the
// two sides never derive names independently. Names are deterministic functions
// of resource identity so a resync produces byte-identical ops.

// NetworkName is the per-project Docker network. Resources in the same project
// share it (no cross-project layer-2 adjacency).
func NetworkName(projectID string) string { return "sigmahub-" + projectID }

// ContainerName is the managed container for a resource.
func ContainerName(resourceID string) string { return "sigmahub-" + resourceID }

// VolumeName is a named Docker volume for a resource's declared volume.
func VolumeName(resourceID, vol string) string {
	return "sigmahub-" + resourceID + "-" + vol
}
