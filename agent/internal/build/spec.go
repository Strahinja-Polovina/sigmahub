// Package build is sigmad's Git deploy build path (P1-9): the typed git.clone and
// image.build ops registered behind the P1-2 apply registry. Cloning a repo at a
// SHA and building an image on the target are FIRST-CLASS TYPED OPS with fixed,
// validated arguments — not a generic shell escape. The clone credential (a
// short-lived provider token) is resolved in agent memory and passed to git via
// the environment, so it is never written to disk or exposed in argv.
package build

// Op kinds registered on both the control plane (reconciler render) and the
// agent (apply registry). Kept in sync with the CP's dsd naming.
const (
	KindGitClone   = "git.clone"
	KindImageBuild = "image.build"
)

// GitCloneSpec is the payload of a git.clone op. CredentialRef, when set, names a
// short-lived clone credential the agent fetches into memory from the control
// plane (never persisted). A public repo needs no credential.
type GitCloneSpec struct {
	ResourceID    string `json:"resourceId"`
	Provider      string `json:"provider"` // github
	RepoFullName  string `json:"repoFullName"`
	Ref           string `json:"ref"`
	SHA           string `json:"sha"`
	CredentialRef string `json:"credentialRef,omitempty"`
}

// BuildImageSpec is the payload of an image.build op. DedupKey lets a retry of
// the same inputs skip the build when the image tag already exists. Dockerfile
// defaults to "Dockerfile" at the context root.
type BuildImageSpec struct {
	ResourceID string `json:"resourceId"`
	SHA        string `json:"sha"`
	DedupKey   string `json:"dedupKey"`
	Dockerfile string `json:"dockerfile,omitempty"`
	// ContextSubdir is a Compose service's build context relative to the cloned
	// repo root (empty ⇒ repo root). Validated to stay within the clone.
	ContextSubdir string `json:"contextSubdir,omitempty"`
	ImageTag      string `json:"imageTag"` // e.g. sigmahub/<resourceId>:<sha>
	// DeploymentID scopes the streamed build logs on the control plane.
	DeploymentID string `json:"deploymentId,omitempty"`
	// Force skips the ImageExists dedup short-circuit so a manual redeploy rebuilds
	// the same commit (picking up base-image changes) instead of reusing the cache.
	Force bool `json:"force,omitempty"`
}
