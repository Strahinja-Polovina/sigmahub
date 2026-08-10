package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

func TestFromEnv(t *testing.T) {
	const db = "postgres://x"
	for _, tc := range []struct {
		name      string
		env       map[string]string
		wantErr   bool
		wantToken string
	}{
		{"dev defaults service token", map[string]string{"CP_DATABASE_URL": db}, false, "dev-service-token"},
		{"missing db", map[string]string{}, true, ""},
		{"prod without static token ok", map[string]string{"CP_DATABASE_URL": db, "CP_ENV": "prod"}, false, ""},
		{"prod rejects static token", map[string]string{"CP_DATABASE_URL": db, "CP_ENV": "prod", "CP_SERVICE_TOKEN": "s3cret"}, true, ""},
		{"unknown env rejected", map[string]string{"CP_DATABASE_URL": db, "CP_ENV": "production"}, true, ""},
		{"unknown env rejected even with token", map[string]string{"CP_DATABASE_URL": db, "CP_ENV": "staging", "CP_SERVICE_TOKEN": "s3cret"}, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{"CP_DATABASE_URL", "CP_ENV", "CP_SERVICE_TOKEN", "CP_ADDR"} {
				t.Setenv(k, "")
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			cfg, err := FromEnv()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got cfg=%+v", cfg)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.ServiceToken != tc.wantToken {
				t.Fatalf("ServiceToken = %q, want %q", cfg.ServiceToken, tc.wantToken)
			}
		})
	}
}

// TestRequireActorFromEnv covers the SIGMA-142 strict boolean parsing: truthy
// spellings enable it, "false"/empty leave it off, and a typo fails boot rather
// than silently leaving the SIGMA-82 strict-mode control disabled (fail-open).
func TestRequireActorFromEnv(t *testing.T) {
	const db = "postgres://x"
	for _, tc := range []struct {
		val     string
		want    bool
		wantErr bool
	}{
		{"", false, false},
		{"true", true, false},
		{"1", true, false},
		{"True", true, false},
		{"false", false, false},
		{"0", false, false},
		{"yes", false, true},
		{"enabled", false, true},
	} {
		t.Run("val="+tc.val, func(t *testing.T) {
			for _, k := range []string{"CP_ENV", "CP_SERVICE_TOKEN", "CP_ADDR", "CP_REQUIRE_ACTOR"} {
				t.Setenv(k, "")
			}
			t.Setenv("CP_DATABASE_URL", db)
			t.Setenv("CP_REQUIRE_ACTOR", tc.val)
			cfg, err := FromEnv()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got cfg=%+v", tc.val, cfg)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.val, err)
			}
			if cfg.RequireActor != tc.want {
				t.Fatalf("RequireActor = %v, want %v", cfg.RequireActor, tc.want)
			}
		})
	}
}

// TestMetricsRetentionFromEnv covers CP_METRICS_RETENTION (SIGMA-257): unset is
// the documented 24h, a duration overrides it, and anything that is not a
// positive Go duration fails boot. The last case is the point — "24" parsing to
// 24 nanoseconds would hand the sweeper a prune window that deletes every sample
// on its next tick, and hand the metrics endpoint a window it would then
// advertise as the truth.
func TestMetricsRetentionFromEnv(t *testing.T) {
	const db = "postgres://x"
	for _, tc := range []struct {
		val     string
		want    time.Duration
		wantErr bool
	}{
		{"", DefaultMetricsRetention, false},
		{"24h", 24 * time.Hour, false},
		{"168h", 7 * 24 * time.Hour, false},
		{"90m", 90 * time.Minute, false},
		{"24", 0, true},
		{"forever", 0, true},
		{"0h", 0, true},
		{"-1h", 0, true},
	} {
		t.Run("val="+tc.val, func(t *testing.T) {
			for _, k := range []string{"CP_ENV", "CP_SERVICE_TOKEN", "CP_ADDR"} {
				t.Setenv(k, "")
			}
			t.Setenv("CP_DATABASE_URL", db)
			t.Setenv("CP_METRICS_RETENTION", tc.val)
			cfg, err := FromEnv()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got %v", tc.val, cfg.MetricsRetention)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.val, err)
			}
			if cfg.MetricsRetention != tc.want {
				t.Fatalf("MetricsRetention = %v, want %v", cfg.MetricsRetention, tc.want)
			}
		})
	}
}

// TestDBEnginesFromEnv covers the P1-10 CP_DB_ENGINES allowlist: unset enables
// every engine the store catalog defines, a subset narrows it (the pre-agreed
// Postgres-only fallback build), and a typo fails boot rather than silently
// disabling an engine the operator believes is on.
//
// The default is compared against store.DBEngineKinds() rather than against a
// spelled-out list, because a spelled-out list here is the third copy SIGMA-216
// deleted from config.go coming back in the test that was meant to defend it.
func TestDBEnginesFromEnv(t *testing.T) {
	const db = "postgres://x"
	for _, tc := range []struct {
		name    string
		val     string // "" ⇒ leave unset (fall through to the default)
		wantErr bool
		want    string // comma-joined expected engines
	}{
		{"unset enables every engine the catalog defines", "", false,
			strings.Join(store.DBEngineKinds(), ",")},
		{"postgres only is the fallback build", "postgres", false, "postgres"},
		{"whitespace tolerated", " postgres , redis ", false, "postgres,redis"},
		{"unknown engine rejected", "postgres,clickhouse", true, ""},
		{"blank list rejected", ",", true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CP_DATABASE_URL", db)
			t.Setenv("CP_S3_ENGINES", "")
			t.Setenv("CP_DB_ENGINES", tc.val)
			cfg, err := FromEnv()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", cfg.DBEngines)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := strings.Join(cfg.DBEngines, ","); got != tc.want {
				t.Fatalf("DBEngines = %q, want %q", got, tc.want)
			}
		})
	}
}

// The rejection message has to be RENDERED from the catalog, not restated beside
// it. config.go used to spell the four database engines out inside the error
// text, next to a map that spelled them out again, next to a default value that
// spelled them out a third time — so an engine added to the control plane
// changed none of them, and the operator who mistyped one was handed the list of
// engines this product supported on whatever day that sentence was last edited.
// Nothing read the sentence, so nothing could notice it had gone stale.
//
// What is asserted is the property (the message names every engine the catalog
// knows), never the wording.
func TestTheUnknownEngineMessageNamesEveryEngineTheCatalogKnows(t *testing.T) {
	for _, tc := range []struct {
		key   string
		known []string
	}{
		{"CP_DB_ENGINES", store.DBEngineKinds()},
		{"CP_S3_ENGINES", store.S3EngineNames()},
	} {
		t.Run(tc.key, func(t *testing.T) {
			t.Setenv("CP_DATABASE_URL", "postgres://x")
			t.Setenv("CP_DB_ENGINES", "")
			t.Setenv("CP_S3_ENGINES", "")
			t.Setenv(tc.key, "clickhouse")
			cfg, err := FromEnv()
			if err == nil {
				t.Fatalf("%s accepted an engine the catalog does not define: %+v", tc.key, cfg)
			}
			for _, engine := range tc.known {
				if !strings.Contains(err.Error(), engine) {
					t.Errorf("%q is in the catalog but %s does not name it: %v", engine, tc.key, err)
				}
			}
		})
	}
}

// TestS3EnginesFromEnv covers the P2-2 CP_S3_ENGINES allowlist: default enables
// both engines, a subset narrows, and a typo fails boot rather than silently
// disabling an engine (mirrors CP_DB_ENGINES).
func TestS3EnginesFromEnv(t *testing.T) {
	const db = "postgres://x"
	for _, tc := range []struct {
		name    string
		val     string // "" ⇒ leave unset (fall through to the default)
		wantErr bool
		want    string // comma-joined expected engines
	}{
		{"default enables both", "", false, "minio,seaweedfs"},
		{"minio only", "minio", false, "minio"},
		{"seaweedfs only", "seaweedfs", false, "seaweedfs"},
		{"whitespace tolerated", " minio , seaweedfs ", false, "minio,seaweedfs"},
		{"unknown engine rejected", "minio,garbage", true, ""},
		{"blank list rejected", ",", true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CP_DATABASE_URL", db)
			t.Setenv("CP_DB_ENGINES", "")
			t.Setenv("CP_S3_ENGINES", tc.val)
			cfg, err := FromEnv()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", cfg.S3Engines)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := strings.Join(cfg.S3Engines, ","); got != tc.want {
				t.Fatalf("S3Engines = %q, want %q", got, tc.want)
			}
		})
	}
}

// The installer proxy's settings (SIGMA-217). The repository slug is
// concatenated into GitHub URLs that a server-side credential is attached to, so
// this is the one of the three that fails boot: an operator-supplied value with
// a scheme, a second slash or a traversal in it would redirect a credentialed
// request, and the routes that use it are unauthenticated.
func TestReleaseRepoMustBeAnOwnerAndNameOrBootFails(t *testing.T) {
	for _, tc := range []struct {
		name    string
		val     string // "" ⇒ leave unset (fall through to the default)
		wantErr bool
		want    string
	}{
		{"unset uses the upstream release repository", "", false, DefaultReleaseRepo},
		{"whitespace tolerated", "  " + DefaultReleaseRepo + "  ", false, DefaultReleaseRepo},
		// A well-formed FORK is refused, and that is the point rather than an
		// omission: install.sh cosign-verifies against DefaultReleaseRepo and
		// the install command carries no trust anchor, so proxying a fork's
		// artifacts produces "cosign verification failed" on the host, after
		// the one-time bootstrap key has been spent. A startup error is the
		// honest form of a promise the install cannot keep.
		{"a well-formed fork is refused, because the script would not trust it", "acme/sigmahub", true, ""},
		{"dots and dashes are legal repository names but still not the anchor", "acme-co/sigma.hub_v2", true, ""},
		{"an owner with no repository", "acme", true, ""},
		{"a trailing path segment", "acme/sigmahub/releases", true, ""},
		{"a full url", "https://github.com/acme/sigmahub", true, ""},
		{"a traversal", "acme/../../etc", true, ""},
		{"a query string", "acme/sigmahub?ref=main", true, ""},
		{"an at-host slug", "acme/sigmahub@evil.example", true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CP_DATABASE_URL", "postgres://x")
			t.Setenv("CP_DB_ENGINES", "")
			t.Setenv("CP_S3_ENGINES", "")
			t.Setenv("CP_RELEASE_REPO", tc.val)
			cfg, err := FromEnv()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("CP_RELEASE_REPO accepted %q, which reaches a credentialed GitHub URL", tc.val)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.ReleaseRepo != tc.want {
				t.Fatalf("ReleaseRepo = %q, want %q", cfg.ReleaseRepo, tc.want)
			}
		})
	}
}

// The token and the version pin are OPTIONAL on purpose: an unmodified
// deployment against a public release must onboard with nothing set, and the
// version falls back to the release this control plane was built from.
func TestTheReleaseCredentialAndVersionPinAreOptional(t *testing.T) {
	t.Setenv("CP_DATABASE_URL", "postgres://x")
	t.Setenv("CP_DB_ENGINES", "")
	t.Setenv("CP_S3_ENGINES", "")
	t.Setenv("CP_RELEASE_REPO", "")
	t.Setenv("CP_RELEASE_TOKEN", "")
	t.Setenv("CP_AGENT_VERSION", "")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("a control plane with no release settings must still boot: %v", err)
	}
	if cfg.ReleaseToken != "" || cfg.AgentVersion != "" {
		t.Fatalf("ReleaseToken = %q, AgentVersion = %q, want both empty", cfg.ReleaseToken, cfg.AgentVersion)
	}

	t.Setenv("CP_RELEASE_TOKEN", "  ghp_x  ")
	t.Setenv("CP_AGENT_VERSION", "  v0.3.0  ")
	cfg, err = FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	// Trimmed, because a token with a stray newline becomes an Authorization
	// header GitHub rejects with a 401 that reads like a permissions problem.
	if cfg.ReleaseToken != "ghp_x" || cfg.AgentVersion != "v0.3.0" {
		t.Fatalf("ReleaseToken = %q, AgentVersion = %q, want both trimmed", cfg.ReleaseToken, cfg.AgentVersion)
	}
}

// The cosign trust anchor and the repository the control plane proxies are one
// fact in two languages, and nothing tied them together.
//
// install.sh bakes SIGMAHUB_REPO in as the certificate-identity the release
// signature is verified against, and deliberately does not accept it from the
// install command — a command carrying its own trust anchor lets whoever wrote
// the command choose who to trust. So if CP_RELEASE_REPO ever names a different
// repository, the control plane proxies artifacts the script will refuse, and
// the operator learns it on the host after the one-time bootstrap key is spent.
func TestTheReleaseRepoIsTheRepositoryInstallShVerifiesAgainst(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "..", "agent", "packaging", "install.sh"))
	if err != nil {
		t.Fatalf("read install.sh: %v", err)
	}
	// The default assignment, as the script writes it:
	//   SIGMAHUB_REPO="${SIGMAHUB_REPO:-owner/name}"
	m := regexp.MustCompile(`SIGMAHUB_REPO="\$\{SIGMAHUB_REPO:-([^}"]+)\}"`).FindSubmatch(src)
	if m == nil {
		t.Fatal("install.sh no longer assigns a default SIGMAHUB_REPO; the cosign trust anchor moved " +
			"and this guard can no longer see it")
	}
	if got := string(m[1]); got != DefaultReleaseRepo {
		t.Errorf("install.sh verifies against %q, DefaultReleaseRepo is %q — the control plane would "+
			"serve one repository's artifacts to a script that trusts another's", got, DefaultReleaseRepo)
	}
}
