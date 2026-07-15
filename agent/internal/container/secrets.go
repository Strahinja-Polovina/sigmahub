package container

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// tmpfsMagic is the Linux statfs f_type for tmpfs. Secrets are only ever written
// where statfs confirms this, so a plaintext value can never reach the on-disk
// rootfs layer even if the dedicated mount was shadowed or absent.
const tmpfsMagic = 0x01021994

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
	dir := fmt.Sprintf("/proc/%d/root%s", pid, SecretsMountDir)
	// The dedicated /run/secrets tmpfs can be absent or shadowed by a parent
	// tmpfs (e.g. a /run mount) depending on the daemon's mount ordering, leaving
	// the directory missing; (re)create it inside whatever is mounted there. When
	// the parent is itself a tmpfs (the case that shadows /run/secrets), the file
	// still lands in RAM.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create secrets dir: %w", err)
	}
	// Hard never-on-disk guarantee: only write where statfs confirms the backing
	// store is tmpfs (RAM). statfs reflects the effective mount at the path, so
	// this holds even if a dedicated secrets tmpfs was shadowed. If the path
	// resolved to the on-disk rootfs, refuse rather than leak plaintext.
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return fmt.Errorf("statfs secrets dir: %w", err)
	}
	if st.Type != tmpfsMagic {
		return fmt.Errorf("%s is not tmpfs-backed in the container (fs=0x%x); refusing to seed secrets to disk", SecretsMountDir, uint64(st.Type))
	}
	for _, s := range secrets {
		name, err := safeSecretFileName(s.Name)
		if err != nil {
			return err
		}
		dst := filepath.Join(dir, name)
		// Defense in depth: the cleaned destination must stay under the secrets dir.
		if !strings.HasPrefix(dst+"/", dir+"/") {
			return fmt.Errorf("secret %q resolves outside %s", s.Name, SecretsMountDir)
		}
		if err := os.WriteFile(dst, []byte(s.Value), 0o444); err != nil {
			return fmt.Errorf("write secret %q: %w", name, err)
		}
	}
	return nil
}
