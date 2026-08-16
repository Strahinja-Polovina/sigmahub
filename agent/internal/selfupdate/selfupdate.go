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

// versionRe is the shape an agent.update target must have before this package
// will build an upstream URL out of it: a goreleaser tag, optionally carrying a
// prerelease suffix.
//
// It is character-for-character the control plane's releaseTagPattern
// (cp/internal/api/installer.go), and that is the whole point (SIGMA-289). The
// two ends decide the same thing about the same string on two machines: the CP
// decides what an operator may ask for and what its /dl proxy will serve, this
// package decides what sigmad will actually install. When they disagreed —
// this pattern used to be `^v\d+\.\d+\.\d+$`, with no prerelease suffix — the
// disagreement was invisible from the control plane and permanent on the host.
// The release workflow sets `prerelease: auto`, so `v0.4.0-rc.1` is a real tag
// the CP serves; canarying it returned 200 "queued", stored
// desired_agent_version, rendered the op and showed an upgrade in flight, while
// every host failed the op with `invalid version`. Because agent_version never
// reaches desired_agent_version the op is re-emitted on every reconcile, so the
// hosts sit in a permanently failing state and the only escape is knowing to
// POST a non-prerelease tag over the top.
//
// Widening rather than narrowing is deliberate: a prerelease is exactly what a
// canary is, and refusing to install one would make the CP's `prerelease: auto`
// releases unreachable through the dashboard's own upgrade path. The gate this
// pattern is here to be does not weaken — `..`, a slash, a query string, a
// space and an absolute URL each require a character it still does not admit.
//
// The two copies are held together by
// agent/internal/selfupdate/version_vocabulary_test.go and
// cp/internal/api/agent_version_vocabulary_test.go, which read each other's
// module off disk (cp and agent are separate Go modules and neither can import
// the other) and fail on the commit that changes either pattern alone.
var versionRe = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)

// The architectures sigmad is published for, and the one place that list is
// written in Go.
//
// It is a named vocabulary rather than a pair of string comparisons because
// three other files restate the same fact and none of them can import this one.
// agent/packaging/install.sh spells it in shell, .goreleaser.yaml spells it as
// the set of assets a release actually builds, and cp/internal/store's server
// catalog spells it as the architectures a host may enroll with — cp and agent
// are separate Go modules and cp cannot import agent, so the citation in that
// catalog's comment could only ever BE a citation.
//
// Drift between any two of the four is an outage in one of two directions. An
// architecture added here and forgotten in the installer is a host sigmad is
// built for and the installer refuses at the distro/arch gate. An architecture
// the installer accepts but the release never built is a download of an asset
// that does not exist — and that failure lands after the one-time bootstrap key
// has already been spent, so the operator's retry needs a whole new token.
//
// agent/packaging/install_script_test.go and
// cp/internal/store/installer_vocabulary_test.go hold the four copies together.
// They read the shell and YAML off disk, which is the only thing two Go modules
// and a shell script can share, and they fail on the edit that introduces the
// drift rather than on the onboarding that reveals it.
var supportedArches = []string{"amd64", "arm64"}

// SupportedArches lists the architectures sigmad is published for, in the order
// the release builds them.
func SupportedArches() []string {
	out := make([]string, len(supportedArches))
	copy(out, supportedArches)
	return out
}

// ArchSupported reports whether a GOARCH value names an architecture sigmad is
// published for.
func ArchSupported(arch string) bool {
	for _, a := range supportedArches {
		if a == arch {
			return true
		}
	}
	return false
}

// ArchiveName renders the release archive for a version and architecture: the
// exact name goreleaser's sigmad archive template produces, and therefore the
// exact name install.sh has to download. Rendered from one function so that
// renaming the artifact is a single Go edit whose shell half the packaging
// drift test names, instead of a rename that self-update and onboarding
// discover separately at runtime.
func ArchiveName(version, arch string) string {
	return fmt.Sprintf("sigmad_%s_linux_%s.tar.gz", strings.TrimPrefix(version, "v"), arch)
}

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
		// NOTE: a "repo" field on the wire is intentionally NOT decoded here. The
		// cosign identity is pinned below (SIGMA-360); letting the spec choose it
		// would let an attacker's repo vouch for an attacker's binary.
		// DownloadBase is the control plane's own release proxy for this exact
		// version — "<cp public url>/dl/<version>" — and when the CP sends one
		// it is the only place the assets are fetched from. See the comment on
		// the base selection below for why this is not optional in practice.
		DownloadBase string `json:"downloadBase"`
	}
	if err := json.Unmarshal(op.Spec, &spec); err != nil {
		return fmt.Errorf("agent.update: bad spec: %w", err)
	}
	if !versionRe.MatchString(spec.Version) {
		return fmt.Errorf("agent.update: invalid version %q", spec.Version)
	}
	// Compared without the leading `v`: the control plane stores and sends
	// `v0.4.0` (its API validates against `^v[0-9]+...`) while this binary's
	// version is stamped by goreleaser as `0.4.0` — the same mismatch TrimPrefix
	// exists for a few lines below when building the download URL. Raw, this guard
	// never fired, so an upgrade op re-rendered into every later document re-ran
	// the whole thing: download, cosign, binary rewrite, os.Exit(0) restart of the
	// root daemon, on every deploy or secret rotation, forever (SIGMA-365).
	if strings.TrimPrefix(spec.Version, "v") == strings.TrimPrefix(u.CurrentVersion, "v") {
		return nil // already there — idempotent no-op
	}
	// The cosign trust anchor is PINNED, never taken from the op spec (SIGMA-360).
	// Letting the document choose the repo its own binary is verified against is no
	// check at all: an attacker publishes a malicious sigmad in a repo they own,
	// cosign keyless-signs it under THAT repo's GitHub-Actions OIDC identity, and a
	// spec-chosen identity regexp would accept it — a root binary swap that passes
	// verification. DownloadBase may still move the bytes (authenticity comes from
	// the pinned identity below), but spec.Repo must not choose the identity.
	repo := u.Repo
	if repo == "" {
		repo = DefaultRepo
	}

	arch := runtime.GOARCH
	if !ArchSupported(arch) {
		// The set is named, not just the refusal. This message is what the
		// dashboard shows against a failed update op, and "unsupported
		// architecture" alone leaves the operator unable to tell a machine we
		// will never publish for from one a release simply has not built yet.
		return fmt.Errorf("agent.update: no sigmad release is published for %q; published architectures are %s",
			arch, strings.Join(supportedArches, ", "))
	}
	if runtime.GOOS != "linux" {
		return fmt.Errorf("agent.update: self-update is linux-only")
	}

	// Where the four assets come from (SIGMA-262).
	//
	// The control plane serves its own release proxy at /dl/<version>/<asset>,
	// and when it tells us about it we use it and nothing else. That route
	// exists because on a PRIVATE release repository the unauthenticated
	// github.com URLs 404: SIGMA-217 moved every ONBOARDING fetch behind the
	// proxy for exactly that reason, but self-update was left pointing at
	// github.com. The result was a fleet that onboarded through the proxy and
	// then could not be upgraded through it — every dashboard-driven upgrade
	// failed with "status 404 Not Found" and the operator's only remaining
	// path was SSH to every host, which is the thing the upgrade button exists
	// to remove.
	//
	// Trust does not move with the bytes. Below, cosign still verifies
	// checksums.txt against the RELEASE REPO's workflow OIDC identity and the
	// archive still has to match that signed checksum, so a control plane that
	// tampered with an asset fails verification here exactly as a tampered
	// github.com would. What the base changes is reachability, not authenticity.
	//
	// Falling back to github.com when the field is absent keeps an agent
	// working against a control plane that predates this field (and against a
	// public release repo, which never needed the proxy).
	base := strings.TrimRight(spec.DownloadBase, "/")
	if base == "" {
		base = fmt.Sprintf("https://github.com/%s/releases/download/%s", repo, spec.Version)
	}
	archive := ArchiveName(spec.Version, arch)

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
