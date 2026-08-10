package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// composeExcludedSettings are the CP_* names the `cp` service must NOT pass, and
// the reason each is left out. An allowlist rather than a silent skip: "this is
// deliberately absent" and "nobody noticed it was absent" look identical in a
// deploy file, and the whole point of the guard below is that they stop looking
// identical.
var composeExcludedSettings = map[string]string{
	"CP_SERVICE_TOKEN": "the dev-only wildcard Org-Admin bypass. FromEnv refuses to boot in prod when it " +
		"is set, so putting it in the production compose file would ship a foot-gun whose only " +
		"correct value is unset",
}

// TestEveryCPSettingIsInTheComposeFile is a guard against the defect class that
// has now cost this repo three separate bugs: a setting the binary reads under
// one name and the deploy files spell under another, or do not spell at all.
//
// The instances so far — the agent version read as SIGMAHUB_AGENT_VERSION
// (SIGMA-269), the Hugging Face token missing from the service entirely
// (e4270d4), the Vault namespace missing (SIGMA-270) — all fail the same way
// from the operator's chair: the value is set in cp/deploy/.env, the container
// never sees it, and the product behaves as if it had never been configured.
// The Vault one is the worst of them, because the failure is not "the feature is
// off": every transit wrap/unwrap silently goes to the ROOT namespace, so a
// fresh install dies on the DSD signing key and an upgrade stops unwrapping the
// pepper and the per-org DEKs — previously stored secrets become unreadable,
// with nothing anywhere naming a namespace as the cause.
//
// Each of those was fixed one at a time. This is the check that makes the next
// one impossible to ship: every CP_* name that appears in the control plane's
// own source must appear in the cp service's environment block, or be listed
// above with a reason.
func TestEveryCPSettingIsInTheComposeFile(t *testing.T) {
	// Both halves of the binary's configuration: FromEnv here, and the handful
	// main.go reads directly (the KMS custody backend, which is resolved before
	// a Config exists).
	named := cpSettingsNamedInSource(t,
		"config.go",
		filepath.Join("..", "..", "cmd", "sigmahub-cp", "main.go"),
	)
	if len(named) == 0 {
		t.Fatal("found no CP_* settings in the control plane's source; the scan moved and this guard " +
			"can no longer see what it is guarding")
	}

	path := filepath.Join("..", "..", "deploy", "docker-compose.yml")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	paths := flattenYAMLScalars(src)

	for _, name := range named {
		if why, excluded := composeExcludedSettings[name]; excluded {
			if _, present := paths["services.cp.environment."+name]; present {
				t.Errorf("%s is passed to the cp service, but it is on the exclusion list: %s", name, why)
			}
			continue
		}
		if _, present := paths["services.cp.environment."+name]; !present {
			t.Errorf("the control plane reads %s, but the cp service in cp/deploy/docker-compose.yml "+
				"does not pass it: an operator who sets it in cp/deploy/.env gets a container that "+
				"never sees it, and a product that behaves as if it were never configured. Add "+
				"`%s: ${%s:-}` to the service (and document it in .env.example), or add it to "+
				"composeExcludedSettings with the reason it must stay out", name, name, name)
		}
	}
}

// cpSettingsNamedInSource returns the CP_* names appearing in the given files'
// string literals, sorted and deduplicated.
//
// Literals, not comments, and parsed rather than grepped: config.go documents
// every setting at length in prose, and a doc comment describing history
// ("CP_SERVICE_TOKEN used to…") is not the binary reading a variable. Literals
// catch both spellings that matter — os.Getenv("CP_X") and the key handed to
// parseBoolEnv/parseDurationEnv — plus the error messages that tell an operator
// which setting to fix, which is the same promise SIGMA-269 was about.
func cpSettingsNamedInSource(t *testing.T, files ...string) []string {
	t.Helper()
	// Anchored on a non-name character so a longer name that merely ENDS in a
	// CP_ sequence (SIGMAHUB_CP_PUBLIC_URL, the dashboard's own setting) is not
	// read as a reference to a control-plane one.
	pattern := regexp.MustCompile(`(^|[^A-Za-z0-9_])(CP_[A-Z][A-Z0-9_]*)`)
	seen := map[string]bool{}
	for _, file := range files {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				s = lit.Value
			}
			for _, m := range pattern.FindAllStringSubmatch(s, -1) {
				seen[m[2]] = true
			}
			return true
		})
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// TestTheComposeExclusionListIsNotStale keeps the allowlist honest from the
// other side: a name excused here that the binary no longer reads is a note
// about a setting that does not exist, and the next reader has no way to tell
// that from a live exclusion.
func TestTheComposeExclusionListIsNotStale(t *testing.T) {
	named := cpSettingsNamedInSource(t,
		"config.go",
		filepath.Join("..", "..", "cmd", "sigmahub-cp", "main.go"),
	)
	for name := range composeExcludedSettings {
		if !slicesContains(named, name) {
			t.Errorf("composeExcludedSettings excuses %s from the compose file, but nothing in the "+
				"control plane's source names it any more", name)
		}
	}
}

func slicesContains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if strings.EqualFold(s, needle) {
			return true
		}
	}
	return false
}
