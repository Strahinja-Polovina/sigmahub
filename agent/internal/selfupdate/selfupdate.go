// Package selfupdate applies the "agent.update" DSD op: dashboard-driven
// sigmad upgrades. The agent downloads the pinned release archive, verifies it
// exactly like the installer does (cosign keyless signature over checksums.txt
// pinned to the release workflow's OIDC identity, then the archive's sha256),
// atomically replaces its own binary, and asks the main loop to restart after
// the op result is reported. systemd (Restart=always) brings the new binary up.
package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/agent/internal/apply"
	"github.com/Strahinja-Polovina/sigmahub/agent/internal/dsd"
)

// Kind is the DSD op kind this package handles.
const Kind = "agent.update"

// DefaultRepo hosts the signed release artifacts.
const DefaultRepo = "Strahinja-Polovina/sigmahub"

var versionRe = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// Updater executes agent.update ops.
type Updater struct {
	Log            *slog.Logger
	CurrentVersion string
	// Repo overrides the release repo (owner/name); empty = DefaultRepo.
	Repo string
	// RequestRestart is invoked after a successful binary swap; the main loop
	// exits once the op status is reported, so the CP sees "applied" before
	// the process is replaced.
	RequestRestart func()

	HTTP *http.Client
}

func (u *Updater) Register(r *apply.Registry) {
	r.Register(Kind, u.handle)
}

func (u *Updater) client() *http.Client {
	if u.HTTP != nil {
		return u.HTTP
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

func (u *Updater) handle(ctx context.Context, op dsd.Op) error {
	var spec struct {
		Version string `json:"version"`
		Repo    string `json:"repo"`
	}
	if err := json.Unmarshal(op.Spec, &spec); err != nil {
		return fmt.Errorf("agent.update: bad spec: %w", err)
	}
	if !versionRe.MatchString(spec.Version) {
		return fmt.Errorf("agent.update: invalid version %q", spec.Version)
	}
	if spec.Version == u.CurrentVersion {
		return nil // already there — idempotent no-op
	}
	repo := spec.Repo
	if repo == "" {
		repo = u.Repo
	}
	if repo == "" {
		repo = DefaultRepo
	}

	arch := runtime.GOARCH
	if arch != "amd64" && arch != "arm64" {
		return fmt.Errorf("agent.update: unsupported architecture %q", arch)
	}
	if runtime.GOOS != "linux" {
		return fmt.Errorf("agent.update: self-update is linux-only")
	}

	base := fmt.Sprintf("https://github.com/%s/releases/download/%s", repo, spec.Version)
	archive := fmt.Sprintf("sigmad_%s_linux_%s.tar.gz", strings.TrimPrefix(spec.Version, "v"), arch)

	work, err := os.MkdirTemp("", "sigmad-update-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)

	files := map[string]string{}
	for _, name := range []string{archive, "checksums.txt", "checksums.txt.sig", "checksums.txt.pem"} {
		p := filepath.Join(work, name)
		if err := u.download(ctx, base+"/"+name, p); err != nil {
			return fmt.Errorf("agent.update: download %s: %w", name, err)
		}
		files[name] = p
	}

	// Same trust chain as the installer: cosign verifies checksums.txt against
	// the release workflow's OIDC identity, the archive verifies against the
	// signed checksum. Refuse to update on any mismatch.
	if err := cosignVerify(ctx, repo, files["checksums.txt"], files["checksums.txt.sig"], files["checksums.txt.pem"]); err != nil {
		return fmt.Errorf("agent.update: %w", err)
	}
	if err := verifyChecksum(files["checksums.txt"], archive, files[archive]); err != nil {
		return fmt.Errorf("agent.update: %w", err)
	}

	bin, err := extractSigmad(files[archive], work)
	if err != nil {
		return fmt.Errorf("agent.update: extract: %w", err)
	}

	self, err := os.Executable()
	if err != nil {
		return err
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return err
	}
	// Atomic swap: write next to the running binary (same filesystem), then
	// rename over it. The running process keeps its old inode; the restart
	// picks up the new one.
	tmp := self + ".next"
	if err := copyFile(bin, tmp, 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmp, self); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("agent.update: install: %w", err)
	}

	u.Log.Info("agent.update: binary replaced; restarting after status report",
		"from", u.CurrentVersion, "to", spec.Version)
	if u.RequestRestart != nil {
		u.RequestRestart()
	}
	return nil
}

func (u *Updater) download(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := u.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %s", resp.Status)
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	// Release archives are a few tens of MB; 512MB bounds a hostile response.
	_, err = io.Copy(f, io.LimitReader(resp.Body, 512<<20))
	return err
}

// cosignVerify shells out to the host cosign (installed at onboarding) with the
// same identity pin the installer uses.
func cosignVerify(ctx context.Context, repo, checksums, sig, pem string) error {
	if _, err := exec.LookPath("cosign"); err != nil {
		return fmt.Errorf("cosign not found on host — cannot verify the release")
	}
	cmd := exec.CommandContext(ctx, "cosign", "verify-blob",
		"--certificate", pem,
		"--signature", sig,
		"--certificate-identity-regexp", fmt.Sprintf(`^https://github\.com/%s/\.github/workflows/release\.yml@`, regexp.QuoteMeta(repo)),
		"--certificate-oidc-issuer", "https://token.actions.githubusercontent.com",
		checksums,
	)
	cmd.Env = append(os.Environ(), "COSIGN_EXPERIMENTAL=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("cosign verification failed: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func verifyChecksum(checksumsPath, name, path string) error {
	sums, err := os.ReadFile(checksumsPath)
	if err != nil {
		return err
	}
	var want string
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == name {
			want = fields[0]
			break
		}
	}
	if want == "" {
		return fmt.Errorf("%s not present in signed checksums.txt", name)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("checksum mismatch for %s", name)
	}
	return nil
}

// extractSigmad pulls the sigmad binary out of the release tar.gz into dir.
func extractSigmad(archivePath, dir string) (string, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		if filepath.Base(hdr.Name) != "sigmad" || hdr.Typeflag != tar.TypeReg {
			continue
		}
		out := filepath.Join(dir, "sigmad.new")
		w, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return "", err
		}
		// The binary is tens of MB; 1GB bounds a hostile archive entry.
		if _, err := io.Copy(w, io.LimitReader(tr, 1<<30)); err != nil {
			w.Close()
			return "", err
		}
		if err := w.Close(); err != nil {
			return "", err
		}
		return out, nil
	}
	return "", fmt.Errorf("sigmad binary not found in %s", filepath.Base(archivePath))
}

func copyFile(src, dest string, mode os.FileMode) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return writeFileAtomicSibling(dest, b, mode)
}

// writeFileAtomicSibling writes dest fully before it exists under its final
// name (write to dest directly is fine here: dest is a fresh ".next" path the
// caller renames over the real target).
func writeFileAtomicSibling(dest string, b []byte, mode os.FileMode) error {
	if err := os.WriteFile(dest, b, mode); err != nil {
		return err
	}
	return os.Chmod(dest, mode)
}

// EnsureHostTools self-heals the host tool set the agent depends on (mesh,
// firewall, backups). Hosts onboarded with a pre-v0.1.1 installer are missing
// wireguard-tools, which silently keeps the WireGuard mesh down and every
// mesh-bound database unschedulable. Best-effort and apt-only; failures are
// logged, never fatal.
func EnsureHostTools(ctx context.Context, log *slog.Logger) {
	if runtime.GOOS != "linux" {
		return
	}
	if _, err := exec.LookPath("apt-get"); err != nil {
		return // non-apt distro: onboarding gates on Ubuntu/Debian, but stay safe
	}
	need := map[string]string{ // binary -> package
		"wg-quick": "wireguard-tools",
		"nft":      "nftables",
		"restic":   "restic",
	}
	var missing []string
	for bin, pkg := range need {
		if _, err := exec.LookPath(bin); err != nil {
			missing = append(missing, pkg)
		}
	}
	if len(missing) == 0 {
		return
	}
	log.Info("host tools missing; installing", "packages", strings.Join(missing, ","))
	update := exec.CommandContext(ctx, "apt-get", "update", "-qq")
	_ = update.Run()
	args := append([]string{"install", "-y", "-qq"}, missing...)
	cmd := exec.CommandContext(ctx, "apt-get", args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		log.Warn("host tool install failed", "err", err, "output", strings.TrimSpace(out.String()))
		return
	}
	log.Info("host tools installed", "packages", strings.Join(missing, ","))
}
