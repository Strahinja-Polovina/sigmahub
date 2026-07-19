package container

import (
	"fmt"
	"strings"
)

// PolicyError is the typed failure a container op returns when the local
// deny-by-default policy rejects a spec. It surfaces to the control plane as
// the op's failure status (state="failed", err=this message), so an operator
// sees exactly which invariant a DSD violated.
type PolicyError struct {
	Rule   string // short machine-ish reason, e.g. "privileged"
	Detail string
}

func (e *PolicyError) Error() string {
	return fmt.Sprintf("policy: %s denied: %s", e.Rule, e.Detail)
}

// CheckPolicy is the agent-local gate applied to every container spec BEFORE it
// reaches Docker, regardless of what the (validly signed) DSD asked for. The
// control plane is trusted to sign DSDs, but the agent still refuses to run a
// workload that breaches these invariants — defence in depth against a CP bug
// or compromise. Returns a *PolicyError on the first violation, nil if allowed.
func CheckPolicy(s ContainerSpec) error {
	switch {
	case s.Privileged:
		return &PolicyError{Rule: "privileged", Detail: "privileged containers are never permitted"}
	case s.HostNetwork:
		return &PolicyError{Rule: "host-network", Detail: "host networking is never permitted; use the project network"}
	case s.HostPID:
		return &PolicyError{Rule: "host-pid", Detail: "the host PID namespace is never permitted"}
	case len(s.HostMounts) > 0:
		return &PolicyError{Rule: "host-mount", Detail: fmt.Sprintf("host-path mounts are never permitted (%d requested); use named volumes", len(s.HostMounts))}
	}
	if err := checkVolumes(s.Volumes); err != nil {
		return err
	}
	if err := checkNetwork(s.Network); err != nil {
		return err
	}
	if err := checkImagePinned(s.Image); err != nil {
		return err
	}
	return nil
}

// managedNamePrefix is the prefix the control plane stamps on every Docker
// object it renders (see dsd naming: networks "sigmahub-<project>",
// "sigmahub-app-<res>"; volumes "sigmahub-<res>-<vol>"; containers). The agent
// treats it as the sole marker of a managed named object so the policy can
// reject anything else without a control-plane round-trip.
const managedNamePrefix = "sigmahub-"

// isManagedName reports whether name is a well-formed managed Docker object name
// and NOT something Docker would reinterpret as a host path or namespace escape.
// The prefix guard makes it managed-only; the character guard is the security-
// critical part — a Binds source that is an absolute path (or contains "..") is
// a host bind mount, not a named volume, and a network name with a slash is not
// a managed network.
func isManagedName(name string) bool {
	if !strings.HasPrefix(name, managedNamePrefix) {
		return false
	}
	if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return false
	}
	return true
}

// checkVolumes closes the host-bind bypass (a VolumeMount.Name is rendered
// verbatim as the Binds SOURCE): Docker treats an absolute-path source as a host
// bind mount, so an unvalidated name like "/var/run/docker.sock" or "/" mounts
// the host socket/root even though HostMounts is rejected. Every mount must name
// a managed volume.
func checkVolumes(vols []VolumeMount) error {
	for _, v := range vols {
		if !isManagedName(v.Name) {
			return &PolicyError{Rule: "volume", Detail: fmt.Sprintf("volume mount source %q is not a managed named volume; host-path binds are never permitted", v.Name)}
		}
	}
	return nil
}

// checkNetwork gates the container's network mode. NetworkMode is a free string
// that Docker interprets, so the HostNetwork bool alone is not enough — a spec
// could set network:"host" (host namespace), "container:<id>" (join another
// container's namespace), or "ns:<path>" and reach the host regardless of the
// HostNetwork flag. Only a named managed network (or the safe "none") is
// allowed; the reserved namespace-sharing modes are refused, AND any name that
// is not managed-prefixed is rejected — otherwise a bare name like "bridge"
// (the shared default bridge) or "sigmahub-<otherProject>" would attach the
// workload to a network it must not reach, defeating per-project isolation.
func checkNetwork(network string) error {
	switch {
	case network == "":
		return &PolicyError{Rule: "network", Detail: "container network must be an explicit managed network"}
	case network == "none":
		return nil // no connectivity — safe (e.g. a portless worker)
	case network == "host":
		return &PolicyError{Rule: "network", Detail: "host networking is never permitted; use the project network"}
	case strings.HasPrefix(network, "container:"):
		return &PolicyError{Rule: "network", Detail: "joining another container's network namespace is not permitted"}
	case strings.HasPrefix(network, "ns:"):
		return &PolicyError{Rule: "network", Detail: "attaching a raw network namespace path is not permitted"}
	}
	if !isManagedName(network) {
		return &PolicyError{Rule: "network", Detail: fmt.Sprintf("network %q is not a managed project network", network)}
	}
	return nil
}

// checkImagePinned rejects floating image references. A digest (`@sha256:…`)
// is ideal (immutable); a specific tag is the interim minimum. A bare
// repository or the `latest` tag is refused so a workload can never silently
// change out from under the pinned DSD. (Image-signature verification against
// a per-org allowlist is a separate, currently-disabled layer — see allowlist.go.)
func checkImagePinned(image string) error {
	if image == "" {
		return &PolicyError{Rule: "image", Detail: "image reference is empty"}
	}
	if strings.Contains(image, "@sha256:") {
		return nil // digest-pinned: immutable
	}
	// Isolate the tag: everything after the last ':' that is not part of a
	// registry host:port (a '/' after the ':' means it was a port, not a tag).
	tag := ""
	if i := strings.LastIndex(image, ":"); i >= 0 && !strings.Contains(image[i+1:], "/") {
		tag = image[i+1:]
	}
	if tag == "" {
		return &PolicyError{Rule: "image", Detail: fmt.Sprintf("image %q is not pinned (no tag or digest)", image)}
	}
	if tag == "latest" {
		return &PolicyError{Rule: "image", Detail: "the floating 'latest' tag is not permitted; pin a version tag or digest"}
	}
	return nil
}
