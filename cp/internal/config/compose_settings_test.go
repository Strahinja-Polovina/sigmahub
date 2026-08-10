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

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
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

// TestDeployFilesDoNotEnumerateEngines keeps the engine allowlists derived
// (SIGMA-268).
//
// parseEngineList holds no engine names on purpose: `known` comes from
// store.DBEngineKinds()/store.S3EngineNames() and the default IS that list, so
// adding an engine to the catalog enables it everywhere and removing one stops
// it being accepted — the three duplicate copies SIGMA-216 deleted are not
// allowed back.
//
// The deploy files reintroduced exactly that enumeration as an explicit value:
// `CP_DB_ENGINES: ${CP_DB_ENGINES:-postgres,mysql,redis,mongodb}` shadows the
// derived default with a list frozen on the day it was typed. Every deployment
// built from the shipped compose file is then pinned to those names, so a fifth
// engine added to the Go catalog — the workflow the generated catalog exists to
// support — is offered by the wizard (which reads the generated catalog) and
// refused at submit time with `database engine "clickhouse" is not enabled on
// this control plane`, after the dialog has closed.
//
// So: the keys stay (see TestEveryCPSettingIsInTheComposeFile — a setting the
// binary reads must be visible in the deploy files), but their values must be
// empty, which is what makes config.go's catalog-derived default apply.
func TestDeployFilesDoNotEnumerateEngines(t *testing.T) {
	engines := append(store.DBEngineKinds(), store.S3EngineNames()...)
	if len(engines) == 0 {
		t.Fatal("the store catalog reports no engines at all; this guard cannot see what it is guarding")
	}

	for _, tc := range []struct {
		file   string
		values map[string]string
	}{
		{"docker-compose.yml", composeEngineSettings(t)},
		{".env.example", envExampleEngineSettings(t)},
	} {
		for _, key := range []string{"CP_DB_ENGINES", "CP_S3_ENGINES"} {
			value, ok := tc.values[key]
			if !ok {
				continue // absent is the other acceptable shape: nothing to shadow.
			}
			for _, engine := range engines {
				if !strings.Contains(value, engine) {
					continue
				}
				t.Errorf("cp/deploy/%s gives %s the value %q, which names the engine %q: that "+
					"list shadows the catalog-derived default in config.go, so every deployment "+
					"built from this file is frozen at the engines that existed when the line was "+
					"typed, and a newly added engine dead-ends at create time. Leave the value "+
					"empty (${%s:-}) and let the catalog decide", tc.file, key, value, engine, key)
				break
			}
		}
	}
}

func composeEngineSettings(t *testing.T) map[string]string {
	t.Helper()
	path := filepath.Join("..", "..", "deploy", "docker-compose.yml")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	out := map[string]string{}
	for _, key := range []string{"CP_DB_ENGINES", "CP_S3_ENGINES"} {
		if v, ok := flattenYAMLScalars(src)["services.cp.environment."+key]; ok {
			out[key] = v
		}
	}
	return out
}

// envExampleEngineSettings reads the uncommented assignments only. A commented
// line is documentation — "Postgres-only fallback build: CP_DB_ENGINES=postgres"
// is an example of a deliberate cut, not a default anything reads.
func envExampleEngineSettings(t *testing.T) map[string]string {
	t.Helper()
	path := filepath.Join("..", "..", "deploy", ".env.example")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	out := map[string]string{}
	for _, raw := range strings.Split(string(src), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if key = strings.TrimSpace(key); key == "CP_DB_ENGINES" || key == "CP_S3_ENGINES" {
			out[key] = strings.TrimSpace(value)
		}
	}
	return out
}
