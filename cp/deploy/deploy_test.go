// Package deploy holds the compose file, the runbook and the smoke check that
// bring a SigmaHub control plane up on a real host — plus the tests that keep
// those files and the CI workflows that drive them agreeing.
//
// There is no Go source here on purpose: nothing in the control-plane binary
// reads these files, GitHub Actions and the staging box do, so the package
// exists only as a home for the checks. It is the same shape
// agent/packaging/install_script_test.go already uses for the installer: when a
// fact is written once in YAML and once in shell and neither can import the
// other, the copy is allowed and the DRIFT is not, so the copy gets a test that
// reads the other file off disk.
//
// What is pinned here is the DEPLOY GATE — which of the stack's paths a rollout
// has to prove before it is allowed to report success. That gate has a specific
// way of failing that no other suite can see: it keeps passing. A check that
// polls a URL nothing in the broken path serves answers 200 while the product
// is down, and a green deploy is worse than no deploy check at all, because it
// is the state in which nobody looks.
package deploy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const (
	deployStagingWorkflow = "../../.github/workflows/deploy-staging.yml"
	releaseWorkflow       = "../../.github/workflows/release.yml"
	goreleaserConfig      = "../../.goreleaser.yaml"
)

func readFileForTest(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// The staging rollout must prove the web→control-plane path before it reports
// success (SIGMA-265).
//
// The gate used to be `/readyz` on the control plane and `/` on the dashboard.
// `/readyz` pings Postgres and nothing else; `/` is the marketing home page,
// which for an anonymous curl renders static sections and never touches the
// control plane, SIGMAHUB_CP_URL or the service token. So the entire class of
// bug that breaks web→CP — the service-token env var renamed, SIGMAHUB_CP_URL
// dropped from the compose environment (which docker-compose.yml already
// records happening once, see the CP_HUGGING_FACE_TOKEN comment), the CP's auth
// middleware tightened — passed the gate with a green tick while every
// logged-in page on staging threw on its first control-plane call.
//
// Two things close it, and this test requires both because each covers what the
// other cannot: smoke.sh drives org → project → environment → enrollment-token
// against the real HTTP surface, and the dashboard health route proves that the
// WEB CONTAINER's own configuration reaches the control plane — a thing no
// curl from the host can observe.
func TestStagingRolloutIsGatedOnTheWebToControlPlanePath(t *testing.T) {
	wf := readFileForTest(t, deployStagingWorkflow)

	if !strings.Contains(wf, "smoke.sh") {
		t.Error("the staging deploy never runs cp/deploy/smoke.sh; the only artifact that " +
			"exercises org → project → environment → enrollment-token provisioning is " +
			"documented as a manual step and therefore never runs")
	}

	// The dashboard poll must name a route that round-trips to the control
	// plane. The bare marketing root is the failure this ticket is about.
	webPoll := regexp.MustCompile(`wait_for web (\S+)`).FindStringSubmatch(wf)
	if webPoll == nil {
		t.Fatal("no `wait_for web <url>` readiness poll in the staging rollout")
	}
	if strings.HasSuffix(webPoll[1], ":3000/") || strings.HasSuffix(webPoll[1], ":3000") {
		t.Errorf("the dashboard readiness gate polls %s — the marketing home page, which "+
			"renders for an anonymous request without touching the control plane, so it "+
			"answers 200 while every authenticated page is broken", webPoll[1])
	}

	// The route the workflow polls has to exist in the dashboard, or the gate
	// is a 404 that curl -sf would fail on for the wrong reason forever.
	if strings.Contains(webPoll[1], "/api/health") {
		if _, err := os.Stat("../../web/src/app/api/health/route.ts"); err != nil {
			t.Errorf("the rollout polls %s but web/src/app/api/health/route.ts does not exist: %v", webPoll[1], err)
		}
	}
}

// Every file goreleaser hashes into checksums.txt must be readable by the
// release job's `sha256sum -c` (SIGMA-271, and SIGMA-156 before it).
//
// goreleaser hashes checksum.extra_files at their SOURCE paths and lists them in
// checksums.txt by basename, but does not copy them into dist/. So the verify
// step has to stage them itself, and when it did that from a hardcoded list —
// a second copy of extra_files — the two forked the moment a fourth file was
// added to one and not the other. The consequence is nastier than a missing
// file: goreleaser has already published the release by then, so the tag ships
// and the workflow goes red, which teaches a maintainer to ignore a red release
// run. That is the state in which a genuinely corrupted artifact goes unnoticed.
//
// The workflow now stages by looking each name in checksums.txt up in the tree,
// so there is no second list to fork. This test runs that staging code — the
// real lines, read out of the workflow — against a synthetic dist/ built from
// the real extra_files, which is the only way to know it resolves all of them
// rather than merely looking like it would.
func TestReleaseVerifyStagesEveryChecksummedExtraFile(t *testing.T) {
	extras := goreleaserExtraFiles(t)
	if len(extras) == 0 {
		t.Fatal("no checksum.extra_files parsed out of .goreleaser.yaml")
	}

	// A tree shaped like the repo at release time: the extra files at their
	// source paths, and a dist/ holding an archive plus checksums.txt.
	root := t.TempDir()
	dist := filepath.Join(root, "dist")
	if err := os.MkdirAll(dist, 0o755); err != nil {
		t.Fatal(err)
	}
	var lines []string
	archive := filepath.Join(dist, "sigmahub-cp_9.9.9_linux_amd64.tar.gz")
	if err := os.WriteFile(archive, []byte("not really a tarball"), 0o644); err != nil {
		t.Fatal(err)
	}
	lines = append(lines, checksumLine(t, archive))
	for _, rel := range extras {
		src := filepath.Join("../..", rel)
		body, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf(".goreleaser.yaml hashes %s, which does not exist: %v", rel, err)
		}
		dst := filepath.Join(root, filepath.Clean(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, body, 0o644); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, checksumLine(t, dst))
	}
	if err := os.WriteFile(filepath.Join(dist, "checksums.txt"),
		[]byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", "-euo", "pipefail", "-c", releaseStagingScript(t))
	cmd.Dir = dist
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the release workflow's verify step cannot check every file it hashes: %v\n%s", err, out)
	}
	for _, rel := range extras {
		name := filepath.Base(rel)
		if _, err := os.Stat(filepath.Join(dist, name)); err != nil {
			t.Errorf("%s is hashed into checksums.txt but never staged into dist/", name)
		}
	}
}

// goreleaserExtraFiles reads the `glob:` entries under checksum.extra_files.
// Hand-parsed rather than through a YAML library: the control plane has no YAML
// dependency and this block is two nesting levels of plain list.
func goreleaserExtraFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	inChecksum, inExtras := false, false
	glob := regexp.MustCompile(`^\s+- glob:\s*(\S+)`)
	for _, line := range strings.Split(readFileForTest(t, goreleaserConfig), "\n") {
		switch {
		case strings.HasPrefix(line, "checksum:"):
			inChecksum = true
		case line != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "#"):
			// A new top-level key ends the block; `release:` has extra_files too
			// and those are NOT hashed, so leaving on this line matters.
			inChecksum, inExtras = false, false
		case inChecksum && strings.Contains(line, "extra_files:"):
			inExtras = true
		case inExtras:
			if m := glob.FindStringSubmatch(line); m != nil {
				out = append(out, m[1])
			} else if strings.TrimSpace(line) != "" && !strings.HasPrefix(strings.TrimSpace(line), "#") {
				inExtras = false
			}
		}
	}
	return out
}

// releaseStagingScript lifts the staging + verification lines out of the release
// workflow so the test exercises what CI will actually run, not a paraphrase of
// it. The cosign verify-blob above them needs a signature this test cannot
// produce, so the extract starts at the staging loop.
func releaseStagingScript(t *testing.T) string {
	t.Helper()
	lines := strings.Split(readFileForTest(t, releaseWorkflow), "\n")
	start, end := -1, -1
	for i, line := range lines {
		if start < 0 && strings.Contains(line, "while read -r _ name") {
			start = i
		}
		if start >= 0 && strings.Contains(line, "sha256sum -c checksums.txt") {
			end = i
			break
		}
	}
	if start < 0 || end < 0 {
		t.Fatalf("could not find the staging loop in %s — if the verify step was rewritten, "+
			"this test must be pointed at the new shape rather than deleted", releaseWorkflow)
	}
	return strings.Join(lines[start:end+1], "\n")
}

func checksumLine(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	// sha256sum's own format: hash, two spaces, name.
	return fmt.Sprintf("%s  %s", hex.EncodeToString(sum[:]), filepath.Base(path))
}

// The container images a self-hoster runs must be signed and carry an SBOM
// (SIGMA-272).
//
// .goreleaser.yaml signs `artifacts: checksum` and generates SBOMs for
// `artifacts: archive` — the Go binary archives, and nothing else. But the
// path README.md puts FIRST for self-hosting is cp/deploy/docker-compose.yml,
// which runs the images this workflow pushes to GHCR, and those went up with
// no signature, no SBOM and no provenance: one `image.source` label was the
// entire chain of custody. The README then closes that section with "Releases …
// are cosign-signed with per-archive SBOMs", which is true of the archives and
// reads, where it sits, as covering the artifact the reader was just told to
// run. So the security review that asks for the signature of
// ghcr.io/…/sigmahub-cp@<digest> gets "no matching signatures".
//
// A signature over a TAG would not close it either: :latest is repointed by the
// next push, so what gets signed has to be the digest each build emits.
func TestPushedContainerImagesAreSignedAndCarryAnSBOM(t *testing.T) {
	wf := readFileForTest(t, deployStagingWorkflow)

	for _, want := range []struct{ needle, why string }{
		{"sbom: true", "the pushed images carry no SBOM, so there is nothing to hand a security review"},
		{"provenance: mode=max", "the pushed images carry no provenance, so nothing ties :latest to a commit"},
		{"cosign sign", "the pushed images are never signed, so cosign verify answers \"no matching signatures\""},
		{"cosign verify", "nothing checks the signature that was just published, which is how a signing step rots unnoticed"},
		{"id-token: write", "the build job cannot mint the OIDC token keyless cosign signing needs"},
	} {
		if !strings.Contains(wf, want.needle) {
			t.Errorf("%q missing from the image build/push job: %s", want.needle, want.why)
		}
	}

	// Signing must cover the digests the build steps emit. Tags move; digests
	// are the bytes someone pulls.
	if strings.Contains(wf, "cosign sign") && !strings.Contains(wf, "outputs.digest") {
		t.Error("cosign signs something other than the build steps' digest outputs — a signature over " +
			"a moving tag says nothing about the image that is later pulled")
	}

	// Both images, not just the control plane: the dashboard is half of what
	// the compose file starts.
	for _, repo := range []string{"CP_IMAGE_REPO", "WEB_IMAGE_REPO"} {
		if !strings.Contains(wf, `cosign sign --yes "$`+repo) {
			t.Errorf("$%s is pushed but never signed", repo)
		}
	}

	// And the README has to hand the reader the command, or the guarantee
	// exists and nobody can act on it.
	readme := readFileForTest(t, "../../README.md")
	for _, needle := range []string{"cosign verify", "ghcr.io/strahinja-polovina/sigmahub-cp", "imagetools inspect"} {
		if !strings.Contains(readme, needle) {
			t.Errorf("README.md never mentions %q — it tells a self-hoster to run the container "+
				"images and documents no way to verify them", needle)
		}
	}
}
