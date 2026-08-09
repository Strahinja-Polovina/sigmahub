// Package packaging holds the host-side scripts and units that ship as release
// assets — the installer, the manual uninstaller, and sigmad's systemd unit —
// together with the tests that keep them agreeing with the Go code they are
// copies of.
//
// There is no Go source here on purpose: nothing in the agent binary reads
// these files, the HOST does, so the package exists only as a home for the
// checks. That is the same shape web/src/lib/decommission.test.ts already uses
// for agent/packaging/uninstall.sh: when two languages must state one fact and
// neither can import the other, the copy is allowed and the DRIFT is not, so
// the copy gets a test that reads the other language off disk.
//
// What this file holds together is the release surface for sigmad's
// architecture vocabulary, which is currently written four times:
//
//	agent/internal/selfupdate  supportedArches — the Go authority
//	agent/packaging/install.sh SUPPORTED_ARCHES — what a new host will accept
//	.goreleaser.yaml           goarch — what a release actually builds
//	cp/internal/store          bothArches — what a host may enroll as
//
// The first three are checked here. The fourth is checked from the control
// plane's own suite (cp/internal/store/installer_vocabulary_test.go), because cp
// and agent are separate Go modules and cp cannot import this one; install.sh is
// the file both modules can read off disk, so it is the hub the two halves are
// pinned to rather than to each other.
//
// The failures being bought are not stylistic. An architecture in selfupdate and
// not in install.sh is a host sigmad is built for and onboarding turns away. An
// architecture in install.sh that the release never built is a 404 on the
// archive download — which happens AFTER the EXIT trap has already spent the
// one-time bootstrap key, so the operator's retry needs a fresh token from the
// dashboard. Both are edits that pass review and every existing suite; both fail
// here on the commit that makes them.
//
// The other thing checked here is WHERE the release assets are fetched from.
// install.sh routes every asset this repository publishes through
// SIGMAHUB_DOWNLOAD_BASE, which the control plane overrides so it can serve
// them with a server-side GitHub credential; that is what makes a private
// release repository onboardable, because an unauthenticated curl at github.com
// 404s on all of them. The variable is defaulted, so an asset fetched from a
// hard-coded URL instead still works on a public repository and fails only for
// the operator the indirection exists for — a silence this suite is the end of.
package packaging

import (
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/agent/internal/selfupdate"
)

const (
	installScriptPath   = "install.sh"
	goreleaserPath      = "../../.goreleaser.yaml"
	sampleVersion       = "v1.2.3"
	sampleVersionNoV    = "1.2.3"
	sampleArch          = "amd64"
	goreleaserArchiveID = "- id: sigmad"
)

// unameSpellings is what `uname -m` calls each architecture the release names
// differently, and it is the one place the two vocabularies are ALLOWED to
// disagree: x86_64 and aarch64 are the kernel's names for the same machines
// Go and the release call amd64 and arm64.
//
// It lives in the test rather than beside supportedArches because the agent
// itself never sees a uname string — it reads runtime.GOARCH, which is already
// the release's spelling. The installer is the only reader, this is the only
// Go-side check of it, and stating it here means a new architecture cannot be
// added to selfupdate without someone deciding what the kernel calls it: the
// first assertion below fails on a missing entry rather than silently checking
// nothing.
var unameSpellings = map[string]string{
	"amd64": "x86_64",
	"arm64": "aarch64",
}

func TestTheInstallerAcceptsExactlyTheArchitecturesSigmadIsPublishedFor(t *testing.T) {
	want := strings.Join(selfupdate.SupportedArches(), " ")
	got := shellVar(t, readInstaller(t), "SUPPORTED_ARCHES")
	if got != want {
		t.Errorf("install.sh accepts %q, sigmad is published for %q.\n"+
			"The installer's list is a copy of selfupdate.SupportedArches; make the line read:\n"+
			"  SUPPORTED_ARCHES=%q", got, want, want)
	}
}

func TestTheInstallerTranslatesEveryUnameSpellingIntoTheReleaseName(t *testing.T) {
	src := readInstaller(t)
	for _, arch := range selfupdate.SupportedArches() {
		spelling, ok := unameSpellings[arch]
		if !ok {
			t.Errorf("no `uname -m` spelling is recorded for %q, so nothing checks that install.sh "+
				"recognises a host of that architecture; add it to unameSpellings in this file", arch)
			continue
		}
		// The normalization arm, not merely the word: install.sh may well
		// mention x86_64 in a comment, and a gate that mentions an
		// architecture without mapping it rejects the host all the same.
		arm := spelling + ") arch=" + arch + " ;;"
		if !strings.Contains(src, arm) {
			t.Errorf("install.sh does not translate `uname -m` %q into %q; it needs the case arm:\n  %s",
				spelling, arch, arm)
		}
	}
}

// Both gates render their rejection from the list they just checked. The
// architecture message always did; the distro one used to read "Onboarding
// supports Ubuntu 22.04/24.04 and Debian 12 only" beside a case arm listing the
// same three ids, which is a sentence that can go stale on its own — correct
// gate, wrong advice, and the operator retypes an install command for an image
// we no longer accept.
func TestTheInstallerRejectsAHostByNamingTheListItChecked(t *testing.T) {
	src := readInstaller(t)
	for _, gate := range []struct{ variable, rejection string }{
		{"SUPPORTED_DISTROS", "unsupported distro"},
		{"SUPPORTED_ARCHES", "unsupported CPU architecture"},
	} {
		line, ok := lineContaining(src, gate.rejection)
		if !ok {
			t.Errorf("install.sh no longer refuses a host with %q; the gate is what makes the "+
				"vocabulary above it mean anything", gate.rejection)
			continue
		}
		if !strings.Contains(line, "${"+gate.variable+"}") {
			t.Errorf("install.sh's %q message does not render ${%s}, so it can go stale while the "+
				"gate stays right:\n  %s", gate.rejection, gate.variable, strings.TrimSpace(line))
		}
		for _, value := range strings.Fields(shellVar(t, src, gate.variable)) {
			if strings.Contains(line, value) {
				t.Errorf("install.sh's %q message spells %q out; it must name only ${%s}:\n  %s",
					gate.rejection, value, gate.variable, strings.TrimSpace(line))
			}
		}
		// And the CHECK has to read the variable too, not merely the sentence
		// printed when it fails. Adversarial review inlined a third
		// architecture into the in_list call while leaving both the variable
		// and the rejection message untouched: every test here passed, and a
		// riscv64 host then cleared the gate, installed, and 404'd on an
		// archive that was never published — after the EXIT trap had already
		// spent the one-time bootstrap token. A gate that agrees with its own
		// error message is the only thing that makes either worth reading.
		check, ok := lineContaining(src, "in_list \"${"+gateVariable(gate.variable)+"}\"")
		if !ok {
			t.Errorf("no in_list check reads ${%s}; the vocabulary above it gates nothing",
				gate.variable)
			continue
		}
		if !strings.Contains(check, "${"+gate.variable+"}") {
			t.Errorf("install.sh's %s gate does not check against ${%s}, so the list it prints "+
				"and the list it enforces are two different things:\n  %s",
				gate.variable, gate.variable, strings.TrimSpace(check))
		}
	}
}

// gateVariable maps a vocabulary variable to the shell variable holding the
// value being tested against it, so the check line can be found without
// matching on the list itself.
func gateVariable(vocabulary string) string {
	if vocabulary == "SUPPORTED_DISTROS" {
		return "distro"
	}
	return "arch"
}

// The installer and self-update download the same asset by name, from two
// languages, and a rename in either one is a 404 rather than a compile error.
func TestTheInstallerDownloadsTheArchiveNameTheAgentAlsoBuilds(t *testing.T) {
	src := readInstaller(t)

	// ArchiveName strips a leading "v"; the shell does it with a parameter
	// expansion, and the two have to strip the same thing or every archive
	// name differs by one character.
	if !strings.Contains(src, `ver_noV="${SIGMAHUB_VERSION#v}"`) {
		t.Error(`install.sh must derive the archive's version as ver_noV="${SIGMAHUB_VERSION#v}", ` +
			"matching the strings.TrimPrefix in selfupdate.ArchiveName")
	}

	raw := shellVar(t, src, "archive")
	got := strings.NewReplacer(
		"${ver_noV}", sampleVersionNoV,
		"${arch}", sampleArch,
	).Replace(raw)
	if want := selfupdate.ArchiveName(sampleVersion, sampleArch); got != want {
		t.Errorf("install.sh downloads %q (%q with the shell expanded), selfupdate.ArchiveName renders %q",
			raw, got, want)
	}
}

func TestTheReleaseBuildsEveryArchitectureTheInstallerWillAccept(t *testing.T) {
	build := yamlListItem(t, yamlSection(readGoreleaser(t), "builds:"), goreleaserArchiveID)

	if got, want := yamlFlowList(t, build, "goarch"), selfupdate.SupportedArches(); !sameSet(got, want) {
		t.Errorf(".goreleaser.yaml builds sigmad for %v, the agent is published for %v.\n"+
			"An architecture built and never installed is dead weight; one installed and never built "+
			"is a 404 on the archive download, after the bootstrap key has been spent.", got, want)
	}

	// goos is a SUPERSET on purpose and is checked loosely for that reason: the
	// release also builds darwin so the binary can be run on a developer's
	// machine, while both install.sh and selfupdate refuse anything but linux.
	// Nothing installs the darwin archive, so it cannot drift into an outage —
	// what would is a release that stopped building linux at all.
	if goos := yamlFlowList(t, build, "goos"); !contains(goos, "linux") {
		t.Errorf(".goreleaser.yaml builds sigmad for %v; onboarding and self-update install linux only", goos)
	}
}

func TestTheReleaseArchiveIsNamedWhatTheInstallerAndSelfUpdateExpect(t *testing.T) {
	archive := yamlListItem(t, yamlSection(readGoreleaser(t), "archives:"), goreleaserArchiveID)
	joined := strings.Join(archive, "\n")

	tmpl := regexp.MustCompile(`name_template:\s*"([^"]*)"`).FindStringSubmatch(joined)
	if tmpl == nil {
		t.Fatalf("the sigmad archive declares no name_template:\n%s", joined)
	}
	got := strings.NewReplacer(
		"{{ .Version }}", sampleVersionNoV,
		"{{ .Os }}", "linux",
		"{{ .Arch }}", sampleArch,
	).Replace(tmpl[1]) + ".tar.gz"
	if want := selfupdate.ArchiveName(sampleVersion, sampleArch); got != want {
		t.Errorf("the release publishes %q, the agent and installer download %q", got, want)
	}
	// The ".tar.gz" above is goreleaser's default and is not stated in the
	// config, so an explicit format override would silently rename every asset
	// while name_template still matched.
	if strings.Contains(joined, "format") {
		t.Errorf("the sigmad archive overrides its format; ArchiveName's .tar.gz suffix is "+
			"goreleaser's default and is not rendered from the config:\n%s", joined)
	}
}

// downloadBase is the expansion install.sh must fetch every one of this
// repository's release assets through.
const downloadBase = "${SIGMAHUB_DOWNLOAD_BASE}"

// releaseAssets is the complete set of assets install.sh downloads from the
// release, written out so that ADDING one is a decision rather than a habit:
// the sixth asset is exactly where a hard-coded github.com URL gets typed, and
// on a public repository it works.
var releaseAssets = []string{
	downloadBase + "/${archive}",
	downloadBase + "/checksums.txt",
	downloadBase + "/checksums.txt.sig",
	downloadBase + "/checksums.txt.pem",
	downloadBase + "/sigmad.service",
}

// thirdPartyDownloads is the complete list of hosts install.sh may fetch from
// directly, and every entry is somebody else's software: sigstore's cosign,
// nixpacks, Docker Engine. They keep their upstream URLs deliberately. Serving
// them through the control plane would make it a mirror for binaries it neither
// builds nor signs, and it would fix nothing — these are public downloads that
// do not 404 on account of THIS repository's visibility, which is the only
// problem SIGMAHUB_DOWNLOAD_BASE exists to solve. A host that cannot reach them
// could not have installed Docker before the base existed either.
var thirdPartyDownloads = []string{
	"https://nixpacks.com/",
	"https://github.com/sigstore/cosign/",
	"https://get.docker.com",
}

// The private-repository outage, written as a check on the file that caused it.
//
// Every asset this repository publishes is fetched through
// SIGMAHUB_DOWNLOAD_BASE, because that variable is the only handle the control
// plane has: the connect-server wizard renders it into the install command
// (web/src/server/actions/servers.ts) and the script follows it. A curl that
// names github.com directly is unauthenticated, and against a private release
// repository it 404s — which is the outage this indirection ended, and it comes
// back one asset at a time.
//
// It comes back INVISIBLY, which is why this is worth a test rather than a
// review comment. The base is defaulted to github.com, so an asset fetched from
// a hard-coded URL behaves identically on a public repository and on every
// developer's machine; it fails only on the operator's private one, halfway
// through an install, with the one-time bootstrap key already spent by the EXIT
// trap.
func TestEveryReleaseAssetIsFetchedThroughTheDownloadBase(t *testing.T) {
	var proxied []string
	for _, line := range strings.Split(readInstaller(t), "\n") {
		trimmed := strings.TrimSpace(line)
		// Comments name these URLs to explain them; the header even renders the
		// install command in full. Only what the shell executes counts.
		if strings.HasPrefix(trimmed, "#") || !strings.Contains(trimmed, "curl ") {
			continue
		}
		for _, url := range downloadTargets(trimmed) {
			switch {
			case strings.HasPrefix(url, downloadBase+"/"):
				proxied = append(proxied, url)
			case isThirdPartyDownload(url):
			default:
				t.Errorf("install.sh fetches %s directly instead of through %s/:\n  %s\n"+
					"A release asset behind a hard-coded URL cannot be served by the control plane, "+
					"so it 404s on a private release repository — the one case the base exists for.",
					url, downloadBase, trimmed)
			}
		}
	}
	if !sameSet(proxied, releaseAssets) {
		t.Errorf("install.sh downloads %v through the base, this test expects %v.\n"+
			"If an asset was added, add it to releaseAssets here; if one was dropped, drop it there.",
			proxied, releaseAssets)
	}
}

// downloadTargets returns the URLs a curl line fetches: absolute ones and those
// built on the download base. The -o destinations are ${work}-relative paths and
// match neither, which is what keeps this from reading a temp file as a host.
func downloadTargets(line string) []string {
	re := regexp.MustCompile(`(\$\{SIGMAHUB_DOWNLOAD_BASE\}[^"'\s]*|https?://[^"'\s]*)`)
	return re.FindAllString(line, -1)
}

func isThirdPartyDownload(url string) bool {
	for _, prefix := range thirdPartyDownloads {
		if strings.HasPrefix(url, prefix) {
			return true
		}
	}
	return false
}

// Both scripts are executed by root on a machine the operator has just handed
// us, and until this file existed nothing in any suite so much as parsed them: a
// stray quote ships in a release asset and the failure is a half-configured host
// with the bootstrap key already spent. `bash -n` is the cheapest possible
// answer and needs no network, no container and no root.
func TestThePackagedScriptsAreValidShell(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		// Deliberately not t.Skip. A check that quietly does not run reads
		// exactly like a check that passed, and both scripts declare
		// #!/usr/bin/env bash, so a machine without bash cannot run them either.
		t.Fatalf("bash is required to parse the packaged scripts: %v", err)
	}
	for _, script := range []string{installScriptPath, "uninstall.sh"} {
		if out, err := exec.Command(bash, "-n", script).CombinedOutput(); err != nil {
			t.Errorf("%s is not valid bash: %v\n%s", script, err, out)
		}
	}
}

func readInstaller(t *testing.T) string  { return readFile(t, installScriptPath) }
func readGoreleaser(t *testing.T) string { return readFile(t, goreleaserPath) }

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// shellVar returns the value of a top-level `NAME="..."` assignment. Anchored to
// the start of a line so a mention inside a comment or a nested command cannot
// be mistaken for the assignment the gate actually reads.
func shellVar(t *testing.T, src, name string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `="([^"]*)"`).FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("install.sh has no top-level %s=\"...\" assignment; the drift check for it "+
			"cannot run, so do not delete the assignment to inline the list", name)
	}
	return m[1]
}

// lineContaining returns the first line holding needle.
func lineContaining(src, needle string) (string, bool) {
	for _, line := range strings.Split(src, "\n") {
		if strings.Contains(line, needle) {
			return line, true
		}
	}
	return "", false
}

// yamlSection returns the lines under a top-level key. Hand-rolled rather than
// unmarshalled because the agent module has no YAML dependency and this check
// must not be the reason it grows one — it reads two flow-style lists and a
// string, which is well inside what a line scan can do honestly.
func yamlSection(src, key string) []string {
	var out []string
	var in bool
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			if in {
				out = append(out, line)
			}
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			continue // a column-0 comment does not end the section
		}
		in = trimmed == key
	}
	return out
}

// yamlListItem returns the lines of one `- id: <name>` entry within a section.
// Both `builds:` and `archives:` contain an entry with the same id, which is
// why the section is selected first.
func yamlListItem(t *testing.T, section []string, marker string) []string {
	t.Helper()
	var out []string
	var in bool
	for _, line := range section {
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "- id:") {
			in = trimmed == marker
		}
		if in {
			out = append(out, line)
		}
	}
	if len(out) == 0 {
		t.Fatalf("no %q entry in that section of .goreleaser.yaml", marker)
	}
	return out
}

// yamlFlowList reads a `key: [a, b]` flow list out of a block.
func yamlFlowList(t *testing.T, block []string, key string) []string {
	t.Helper()
	m := regexp.MustCompile(regexp.QuoteMeta(key) + `:\s*\[([^\]]*)\]`).
		FindStringSubmatch(strings.Join(block, "\n"))
	if m == nil {
		t.Fatalf("no %s: [...] in:\n%s", key, strings.Join(block, "\n"))
	}
	var out []string
	for _, item := range strings.Split(m[1], ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
