package config

import (
	"strings"
	"testing"
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
