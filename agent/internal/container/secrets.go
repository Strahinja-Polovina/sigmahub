package container

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SecretFetcher resolves a resource's secrets from the control plane at
// container-create time. It returns decrypted values that are injected and then
// dropped — never persisted to the desired-state store.
type SecretFetcher func(ctx context.Context, resourceID string) ([]Secret, error)

// safeSecretFileName validates that a secret name is a plain filename safe to
// place under SecretsMountDir. Because file-mode secrets are written through the
// container's mount namespace (see writeFileSecrets), a name containing a path
// separator or ".." could escape the tmpfs and land the plaintext on the
// container's on-disk layer (or overwrite an arbitrary file). The control plane
// already constrains names to identifiers, but the agent re-checks: a signed DSD
// is not a license to skip local validation.
func safeSecretFileName(name string) (string, error) {
	if name == "" || name == "." || name == ".." {
		return "", fmt.Errorf("invalid secret name %q", name)
	}
	if strings.ContainsAny(name, "/\\\x00") {
		return "", fmt.Errorf("secret name %q must not contain a path separator", name)
	}
	if name != filepath.Base(name) {
		return "", fmt.Errorf("secret name %q must be a plain filename", name)
	}
	return name, nil
}

// writeFileSecrets seeds file-mode secrets into the container's tmpfs by writing
// them through the running container's mount namespace at
// /proc/<pid>/root/<SecretsMountDir>. This is the ONLY mechanism that reliably
// lands the bytes in the in-memory tmpfs (RAM) and never on the host disk layer:
// docker cp (PUT .../archive) writes to the graphdriver layer on the
// containerd-snapshotter storage driver, leaving the tmpfs empty AND leaking the
// plaintext to disk. Writing via /proc goes straight into the live mount
// namespace the workload sees. Requires the agent to share the host PID
// namespace (it runs as a host daemon) and the container to be running (pid>0).
//
// Files are 0444: the agent writes as root (uid 0), so a 0400 file would be
// unreadable by a non-root workload; the value is confined to this one
// container's mount namespace, so readable-within-the-container is the right
// trade-off (mirrors Docker Swarm secret semantics).
func writeFileSecrets(pid int, secrets []Secret) error {
	if pid <= 0 {
		return fmt.Errorf("container has no pid; cannot seed secret files")
	}
	root := fmt.Sprintf("/proc/%d/root", pid)
	dir := filepath.Join(root, SecretsMountDir)
	for _, s := range secrets {
		name, err := safeSecretFileName(s.Name)
		if err != nil {
			return err
		}
		dst := filepath.Join(dir, name)
		// Defense in depth: the cleaned destination must stay under the secrets
		// dir even if validation missed something.
		if dst != filepath.Join(dir, name) || !strings.HasPrefix(dst+"/", dir+"/") {
			return fmt.Errorf("secret %q resolves outside %s", s.Name, SecretsMountDir)
		}
		if err := os.WriteFile(dst, []byte(s.Value), 0o444); err != nil {
			return fmt.Errorf("write secret %q: %w", name, err)
		}
	}
	return nil
}
