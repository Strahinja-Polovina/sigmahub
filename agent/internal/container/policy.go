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
	if err := checkImagePinned(s.Image); err != nil {
		return err
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
