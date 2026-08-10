package api

// SIGMA-289: the control plane must not accept an agent version the agent will
// refuse to install.
//
// `agent.update` is a two-ended contract. This module decides what a caller may
// ask for (releaseTagPattern, which is also what the /dl proxy will serve), and
// agent/internal/selfupdate decides what sigmad will actually install
// (versionRe). Nothing tied the two spellings together, so a tag the CP
// accepted and rendered could be one every agent in the fleet rejected — and
// the failure is invisible from the control plane: the POST returns 200
// "queued", desired_agent_version is stored, the op renders, the dashboard says
// an upgrade is in flight, and every host fails it forever because
// agent_version never converges on desired_agent_version.
//
// cp and agent are separate Go modules and cp cannot import agent, so this
// reads the pattern out of the agent's source off disk, exactly as
// cp/internal/store/installer_vocabulary_test.go reads agent/packaging/
// install.sh. agent/internal/selfupdate/version_vocabulary_test.go is the
// mirror of this test, so the drift fails in whichever module introduces it.

import (
	"os"
	"regexp"
	"testing"
)

// agentSelfupdatePath is agent/internal/selfupdate/selfupdate.go relative to
// this package's directory (tests run with the package dir as cwd).
const agentSelfupdatePath = "../../../agent/internal/selfupdate/selfupdate.go"

// agentVersionReLiteral pulls the regexp source out of the agent's
// `var versionRe = regexp.MustCompile(`...`)` declaration.
var agentVersionReLiteral = regexp.MustCompile("versionRe\\s*=\\s*regexp\\.MustCompile\\(`([^`]*)`\\)")

func agentVersionPattern(t *testing.T) *regexp.Regexp {
	t.Helper()
	src, err := os.ReadFile(agentSelfupdatePath)
	if err != nil {
		t.Fatalf("read agent selfupdate source: %v", err)
	}
	m := agentVersionReLiteral.FindSubmatch(src)
	if m == nil {
		t.Fatalf("no `versionRe = regexp.MustCompile(...)` in %s — the agent's version vocabulary moved and this test can no longer pin it", agentSelfupdatePath)
	}
	re, err := regexp.Compile(string(m[1]))
	if err != nil {
		t.Fatalf("agent versionRe %q does not compile: %v", m[1], err)
	}
	return re
}

// tagVocabulary is the sample set the two patterns must agree on. It is not
// exhaustive — it does not need to be. It carries the release shapes that
// actually occur (goreleaser tags, and the prereleases `prerelease: auto`
// publishes) plus the malformed segments releaseTagPattern exists to refuse.
var tagVocabulary = []string{
	"v0.1.2",
	"v0.4.0",
	"v10.20.30",
	// The tag that split the two ends: the CP served it, the agent refused it.
	"v0.4.0-rc.1",
	"v0.4.0-beta",
	"v1.0.0-alpha.2",
	"v1.0.0-rc-1",
	// Malformed / hostile — both ends must refuse all of these.
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

func TestControlPlaneAndAgentAcceptTheSameAgentVersions(t *testing.T) {
	agentRe := agentVersionPattern(t)
	for _, tag := range tagVocabulary {
		cpOK := releaseTagPattern.MatchString(tag)
		agentOK := agentRe.MatchString(tag)
		if cpOK == agentOK {
			continue
		}
		if cpOK {
			t.Errorf("the control plane accepts %q and the agent refuses it (%s).\n"+
				"POST /agent-update would answer 200 \"queued\", render the op, and every host in the fleet would fail it forever — "+
				"agent_version never reaches desired_agent_version, so the op is re-emitted on every reconcile and the dashboard shows an upgrade that cannot finish.",
				tag, agentRe)
		} else {
			t.Errorf("the agent would install %q and the control plane refuses to ask for it (%s).\n"+
				"That direction is only a dead end rather than a wedged fleet, but the two ends still have to spell one vocabulary.",
				tag, releaseTagPattern)
		}
	}
}
