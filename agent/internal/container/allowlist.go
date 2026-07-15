package container

import "strings"

// ImageAllowlist is the per-org image-signature allowlist enforcement point.
//
// It ships DISABLED and is a deliberate, recorded staging of the security bar,
// not a silent hook: locally BuildKit-built application images are unsigned
// until SLSA provenance lands (M6–M9), so enforcing signatures now would block
// every real deploy. The schema and the single enforcement call site
// (Check, invoked from the image.pull handler) ship today so that turning the
// bar on later is a config flip plus a populated allowlist — no new plumbing.
// The deferral requires recorded security-consultant sign-off in the P1-3
// design review.
type ImageAllowlist struct {
	// Enabled gates enforcement. When false (the shipped default) Check always
	// allows, so unsigned locally-built images run.
	Enabled bool `json:"enabled"`
	// Prefixes are the repository prefixes whose images are permitted once
	// enforcement is enabled (e.g. "ghcr.io/acme/", "docker.io/library/").
	Prefixes []string `json:"prefixes,omitempty"`
}

// Check reports whether an image may be pulled. With enforcement disabled it
// always allows. With enforcement enabled it requires the reference to match an
// allowlisted prefix (signature verification against the matched entry is the
// next layer, wired here when provenance lands).
func (a ImageAllowlist) Check(image string) error {
	if !a.Enabled {
		return nil
	}
	for _, p := range a.Prefixes {
		if strings.HasPrefix(image, p) {
			return nil
		}
	}
	return &PolicyError{Rule: "image-allowlist", Detail: "image is not on the org signature allowlist"}
}
