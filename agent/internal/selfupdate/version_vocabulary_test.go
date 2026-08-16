package selfupdate

// SIGMA-289: sigmad must install exactly the versions the control plane is
// allowed to ask for.
//
// The `agent.update` op is a two-ended contract and the two ends used to spell
// the version vocabulary differently. The CP admits a goreleaser tag with an
// optional prerelease suffix — the release workflow sets `prerelease: auto`, so
// `v0.4.0-rc.1` is a tag its own /dl proxy will happily serve — while this
// package refused anything but `vN.N.N`. Canarying a release candidate onto a
// few hosts therefore looked fine from the dashboard (200 "queued", op
// rendered, upgrade "in flight") and failed on every host with `invalid
// version`, forever: agent_version never reaches desired_agent_version, so the
// renderer re-emits the op on every reconcile and the only escape is knowing to
// POST a non-prerelease tag over the top.
//
// cp and agent are separate Go modules and neither can import the other, so
// this reads the CP's pattern out of its source off disk — the same move
// cp/internal/store/installer_vocabulary_test.go makes on
// agent/packaging/install.sh. cp/internal/api/agent_version_vocabulary_test.go
// is the mirror of this test, so drift fails in whichever module introduces it.

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/agent/internal/dsd"
)

// cpInstallerPath is cp/internal/api/installer.go relative to this package's
// directory (tests run with the package dir as cwd).
const cpInstallerPath = "../../../cp/internal/api/installer.go"

// cpReleaseTagLiteral pulls the regexp source out of the control plane's
// `var releaseTagPattern = regexp.MustCompile(`...`)` declaration.
var cpReleaseTagLiteral = regexp.MustCompile("releaseTagPattern\\s*=\\s*regexp\\.MustCompile\\(`([^`]*)`\\)")

func cpReleaseTagPattern(t *testing.T) *regexp.Regexp {
	t.Helper()
	src, err := os.ReadFile(cpInstallerPath)
	if err != nil {
		t.Fatalf("read control-plane installer source: %v", err)
	}
	m := cpReleaseTagLiteral.FindSubmatch(src)
	if m == nil {
		t.Fatalf("no `releaseTagPattern = regexp.MustCompile(...)` in %s — the CP's version vocabulary moved and this test can no longer pin it", cpInstallerPath)
	}
	re, err := regexp.Compile(string(m[1]))
	if err != nil {
		t.Fatalf("control-plane releaseTagPattern %q does not compile: %v", m[1], err)
	}
	return re
}

// tagVocabulary mirrors the sample set in the control plane's copy of this
// test: the release shapes that actually occur, plus the malformed segments
// both ends must refuse.
var tagVocabulary = []string{
	"v0.1.2",
	"v0.4.0",
	"v10.20.30",
	"v0.4.0-rc.1",
	"v0.4.0-beta",
	"v1.0.0-alpha.2",
	"v1.0.0-rc-1",
	"",
	"0.4.0",
	"v1.2",
	"v1.2.3.4",
	"v1.2.3+meta",
	"v1.2.3-",
	"latest",
	"../../etc/passwd",
	"v1.2.3/../v9.9.9",
	"v1.2.3%2e%2e",
	"https://evil.example/x",
	"v1.2.3 rc1",
}

func TestAgentInstallsExactlyTheVersionsTheControlPlaneServes(t *testing.T) {
	cpRe := cpReleaseTagPattern(t)
	for _, tag := range tagVocabulary {
		cpOK := cpRe.MatchString(tag)
		agentOK := versionRe.MatchString(tag)
		if cpOK == agentOK {
			continue
		}
		if cpOK {
			t.Errorf("the control plane accepts %q and this package refuses it (%s).\n"+
				"That is a fleet stuck on an upgrade the dashboard says is in flight and no host will ever apply.", tag, versionRe)
		} else {
			t.Errorf("this package would install %q and the control plane refuses to ask for it (%s).", tag, cpRe)
		}
	}
}

// TestPrereleaseIsAValidUpdateTarget is the behavioural half: the op handler
// itself must not reject a prerelease tag. CurrentVersion is set to the target
// so the handler short-circuits on its idempotent no-op instead of reaching the
// network — the version gate runs before that check, so a refusal still shows.
func TestPrereleaseIsAValidUpdateTarget(t *testing.T) {
	const target = "v0.4.0-rc.1"
	u := &Updater{CurrentVersion: target}
	spec, err := json.Marshal(map[string]string{"version": target})
	if err != nil {
		t.Fatal(err)
	}
	if err := u.handle(context.Background(), dsd.Op{ID: "agent:update:srv_1:" + target, Kind: Kind, Spec: spec}); err != nil {
		t.Fatalf("agent.update %s: %v — the control plane accepts this tag and serves it from /dl", target, err)
	}
}

// The other half of the same vocabulary gap (SIGMA-365).
//
// goreleaser stamps `main.version` from `{{ .Version }}`, which is the tag MINUS
// the leading `v` — the reason install.sh computes `ver_noV="${SIGMAHUB_VERSION#v}"`
// and the reason downloadURL below TrimPrefixes before naming the archive. The
// control plane validates the desired version against `^v[0-9]+...`, so the op
// always carries the `v` form.
//
// Compared raw, this guard could never fire. Since the control plane's own
// re-render check had the same shape, the upgrade op rode in every subsequent
// document and this handler ran it every time: a ~30 MB download, a cosign
// verification, a binary rewrite and an os.Exit(0) restart of the root daemon on
// a customer's machine — on every deploy, secret rotation or domain attach.
//
// The guard is the last thing standing between that op and the customer's host,
// so it is asserted against the spellings that actually occur rather than the
// ones that would be tidy.
func TestUpdateIsANoOpWhenAlreadyOnTheTargetVersion(t *testing.T) {
	// `asked` is always v-prefixed: it comes from the control plane, whose API
	// refuses anything else, and versionRe here enforces the same on the wire.
	// What varies is how THIS binary spells its own version.
	for _, tc := range []struct{ current, asked string }{
		{"0.4.0", "v0.4.0"}, // the real pairing: stamped bare, asked v-prefixed
		{"v0.4.0", "v0.4.0"},
	} {
		u := &Updater{CurrentVersion: tc.current}
		spec, err := json.Marshal(map[string]string{"version": tc.asked})
		if err != nil {
			t.Fatal(err)
		}
		// No DownloadBase and no network available here: if the guard fails to
		// short-circuit, the handler reaches out and this errors — which is the
		// signal, since in production it would instead succeed and replace the
		// running binary.
		if err := u.handle(context.Background(), dsd.Op{
			ID: "agent:update:srv_1:" + tc.asked, Kind: Kind, Spec: spec,
		}); err != nil {
			t.Errorf("current %q asked %q: %v — the agent is already on this version and must do nothing",
				tc.current, tc.asked, err)
		}
	}
}
