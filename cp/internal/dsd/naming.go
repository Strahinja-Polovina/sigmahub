package dsd

import "strings"

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
	KindGitClone       = "git.clone"
	KindImageBuild     = "image.build"
	KindDeployRollout  = "deploy.rollout"
	KindDeployRecreate = "deploy.recreate"
	// Backups (P1-11): engine-native dump piped into restic, restic check +
	// GFS forget, automated restore-verify into a scratch container, and the
	// fire-drill restore into a fresh resource. Names must match the agent's
	// backup package byte-for-byte.
	KindBackupRun     = "backup.run"
	KindBackupVerify  = "backup.verify"
	KindBackupRestore = "backup.restore"
	// P2-5: daily physical base backup (pg_basebackup → restic), the PITR
	// starting point WAL segments replay from.
	KindBackupBase = "backup.base"
	// P2-5b (SIGMA-67): restore-to-timestamp. Recover a fresh resource to a
	// chosen point in time — restore the newest base backup taken before the
	// target, replay WAL up to recovery_target_time, then load the recovered
	// state into the fresh resource.
	KindBackupRestorePITR = "backup.restore-pitr"
	// S3 bucket/key/quota CRUD + storage metering (SIGMA-65): the on-demand op
	// the agent runs against a provisioned MinIO/SeaweedFS engine (create/delete
	// a bucket, set a quota, mint a per-bucket key, measure usage). Name must
	// match the agent's s3ops package byte-for-byte.
	KindS3Configure = "s3.configure"
)

// DeployImageTag is the deterministic local image tag for a resource at a SHA,
// so a clone/build/rollout chain and a rollback reference the same image.
//
// Bare (unpinned) tags are MUTABLE: a manual redeploy of the same commit
// rebuilds the tag in place, so a rollback rendered from one silently re-ships
// whatever the tag points at now (SIGMA-173). New builds therefore use the
// pinned variants below; the bare form remains only for legacy rows recorded
// before the pin existed.
func DeployImageTag(resourceID, sha string) string {
	return "sigmahub/" + resourceID + ":" + sha
}

// DeployServiceImageTag is the per-service image tag for a Compose service, so
// each service in a multi-service resource builds and runs its own image.
// Mutable like DeployImageTag — see the pinned variants.
func DeployServiceImageTag(resourceID, service, sha string) string {
	return "sigmahub/" + resourceID + "-" + service + ":" + sha
}

// DeployPin is a deployment's build pin: the short unique suffix under which
// that deployment's images are tagged. Because every deployment builds under
// its own pin, a tag is never rebuilt in place, and a rollback re-ships the
// exact bytes of the release it names — not whatever a shared SHA tag has
// mutated into since (SIGMA-173). Same derivation as the rollout generation
// suffix: the trailing id segment, capped at 6 chars.
func DeployPin(deploymentID string) string {
	pin := deploymentID
	if i := strings.LastIndex(pin, "_"); i >= 0 {
		pin = pin[i+1:]
	}
	if len(pin) > 6 {
		pin = pin[len(pin)-6:]
	}
	return pin
}

// PinnedImageTag is the immutable per-deployment image tag: the SHA tag plus
// the build pin of the deployment that built it.
func PinnedImageTag(resourceID, sha, pin string) string {
	if pin == "" {
		return DeployImageTag(resourceID, sha)
	}
	return DeployImageTag(resourceID, sha) + "-" + pin
}

// PinnedServiceImageTag is PinnedImageTag for one Compose service. The pin
// makes a Compose release's per-service images re-derivable from its
// deployment row alone, which is what lets a Compose rollback render
// rollout-only ops instead of re-cloning from git (SIGMA-168).
func PinnedServiceImageTag(resourceID, service, sha, pin string) string {
	if pin == "" {
		return DeployServiceImageTag(resourceID, service, sha)
	}
	return DeployServiceImageTag(resourceID, service, sha) + "-" + pin
}

// ServiceContainerName is the base container name for a Compose service (the
// rollout/recreate op suffixes it with the generation).
func ServiceContainerName(resourceID, service string) string {
	return "sigmahub-" + resourceID + "-" + service
}

// TraefikRouterName is the deterministic Traefik router/service name for a
// resource, so a resync renders byte-identical labels. Used by resources that
// are replaced in place (a registry-image app): only one container ever carries
// these labels, so the name needs no generation.
func TraefikRouterName(resourceID string) string { return "sigmahub-" + resourceID }

// TraefikGenerationRouterName is the router/service name for ONE generation of a
// blue-green deployed app. Two generations coexist during a swap; sharing a
// router+service name made Traefik merge them into a single weighted service and
// send live traffic to the new container the instant it started — before the
// health gate ran (SIGMA-164). Scoping the name to the generation gives each its
// own router and its own single-server service, so which one serves is decided
// by router priority (see reconciler.generationRouterPriority) rather than by a
// round-robin over both.
func TraefikGenerationRouterName(resourceID, generation string) string {
	if generation == "" {
		return TraefikRouterName(resourceID)
	}
	return "sigmahub-" + resourceID + "-" + generation
}

// Docker object naming. The control plane is the sole authority for these
// names; it renders them into DSD ops and the agent uses them verbatim, so the
// two sides never derive names independently. Names are deterministic functions
// of resource identity so a resync produces byte-identical ops.

// NetworkName is the per-project Docker network. Resources in the same project
// share it (no cross-project layer-2 adjacency).
func NetworkName(projectID string) string { return "sigmahub-" + projectID }

// ResourceNetworkName is the per-resource Docker network a Compose app's services
// share. Compose service discovery uses bare service names as network aliases
// ("db", "web") — a dedicated network per compose app (docker-compose semantics)
// keeps those aliases from colliding across apps in the same project. Traefik
// still reaches the web-facing service: the proxy attaches to every managed
// network.
func ResourceNetworkName(resourceID string) string { return "sigmahub-app-" + resourceID }

// ContainerName is the managed container for a resource.
func ContainerName(resourceID string) string { return "sigmahub-" + resourceID }

// VolumeName is a named Docker volume for a resource's declared volume.
func VolumeName(resourceID, vol string) string {
	return "sigmahub-" + resourceID + "-" + vol
}
