package store

// The onboarding installer is a fourth copy of two of this package's
// vocabularies, and this file is what holds it to them.
//
// agent/packaging/install.sh decides, on the host, whether a machine may be
// onboarded at all: it reads /etc/os-release and `uname -m` and refuses anything
// outside two lists it spells in shell. This package decides the same two things
// on the control-plane side — supportedDistroLabels is what DistroSupported and
// the SIGMA-203 compatibility gate enroll against, bothArches is what a server
// type's Requires.Arches is drawn from. Neither side can read the other: cp and
// agent are separate Go modules and cp cannot import agent, which is why the
// catalog's citation of agent/internal/selfupdate is a comment and not an
// import.
//
// So the copies stay, and the drift is what gets deleted. install.sh is the one
// file BOTH Go modules can read off disk, so it is the hub: this test pins the
// catalog to it, and agent/packaging/install_script_test.go pins
// selfupdate.SupportedArches and .goreleaser.yaml's build matrix to the same
// line. Four statements of one fact, held together transitively, each failing on
// the commit that changes it.
//
// That is the same move web/src/lib/decommission.test.ts already makes for
// agent/packaging/uninstall.sh, and it is worth more here than any refactor
// would be, because the failures it buys are not detectable at any layer that
// could be refactored:
//
//   - a distro added here and not in install.sh is a host the dashboard promises
//     to onboard and the installer refuses on the first line of the script,
//     after the operator has already pasted a command carrying a bootstrap token;
//   - a distro added to install.sh and not here is worse, because it works: the
//     agent installs, registers, heartbeats, and the compatibility gate then
//     parks the server as `incompatible` — a fully provisioned machine the
//     product will not schedule onto and cannot explain;
//   - an architecture out of step in either direction is one of those two,
//     plus a release asset nobody downloads or a download of an asset that was
//     never published.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// installScriptPath is agent/packaging/install.sh, relative to this package's
// directory (tests run with the package dir as cwd). Reading across the module
// boundary is the point of the file; it needs no network and no container, so
// it runs everywhere `go test ./...` does.
const installScriptPath = "../../../agent/packaging/install.sh"

func TestTheInstallerOnboardsExactlyTheDistrosTheCatalogEnrolls(t *testing.T) {
	want := strings.Join(SupportedDistros(), " ")
	got := installerList(t, "SUPPORTED_DISTROS")
	if got != want {
		t.Errorf("install.sh onboards %q, the catalog enrolls %q.\n"+
			"These are the same decision made on two machines and they must agree exactly; "+
			"make the line in agent/packaging/install.sh read:\n  SUPPORTED_DISTROS=%q", got, want, want)
	}
}

// bothArches is a copy of agent/internal/selfupdate's list — a copy the catalog
// cites in a comment and cannot import. install.sh states the same list a third
// time, and the agent module pins it to selfupdate, so agreeing with the shell
// is how this package agrees with the agent.
func TestTheInstallerAcceptsExactlyTheArchitecturesAHostCanEnrollWith(t *testing.T) {
	want := strings.Join(bothArches, " ")
	got := installerList(t, "SUPPORTED_ARCHES")
	if got != want {
		t.Errorf("install.sh accepts %q, the catalog enrolls %q.\n"+
			"agent/packaging/install_script_test.go holds that same line to "+
			"selfupdate.SupportedArches, so this failure means the catalog and the agent "+
			"now disagree about which machines can run sigmad at all", got, want)
	}
}

// A type may narrow the architecture list — gpu does, to amd64, because the
// pinned inference runtime publishes x86_64 layers only. What it may not do is
// widen it: an architecture no host can install the agent on is a requirement
// the connect dialog would advertise and no machine could ever satisfy.
func TestEveryTypeNarrowsItsArchitecturesFromTheInstallableSet(t *testing.T) {
	for _, spec := range ServerCatalog() {
		for _, arch := range spec.Requires.Arches {
			if !contains(bothArches, arch) {
				t.Errorf("%s enrolls on %q, which agent/packaging/install.sh will not install sigmad on",
					spec.Type, arch)
			}
		}
	}
	// The narrowing that exists today, asserted so that removing it is a
	// decision rather than an accident: see the comment on the gpu entry.
	gpu, ok := ServerRequirementsFor("gpu")
	if !ok {
		t.Fatal("gpu is not in the catalog")
	}
	if equalSets(gpu.Arches, bothArches) {
		t.Error("a gpu host must not enroll on arm64: the pinned inference runtime ships x86_64 " +
			"layers only, so the host would enroll and then fail its first image pull")
	}
}

// installerList reads a top-level NAME="..." assignment out of install.sh.
// Anchored to the start of a line so a mention in a comment cannot stand in for
// the assignment the gate actually reads.
func installerList(t *testing.T, name string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `="([^"]*)"`).
		FindStringSubmatch(readInstallScript(t))
	if m == nil {
		t.Fatalf("install.sh has no top-level %s=\"...\" assignment; inlining the list back into "+
			"the case arms removes the only thing checking it against this catalog", name)
	}
	return m[1]
}

func readInstallScript(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(installScriptPath)
	if err != nil {
		t.Fatalf("read %s: %v", installScriptPath, err)
	}
	return string(b)
}
