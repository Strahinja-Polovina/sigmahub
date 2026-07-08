package config

import "testing"

func TestFromEnv(t *testing.T) {
	const db = "postgres://x"
	for _, tc := range []struct {
		name         string
		env          map[string]string
		wantErr      bool
		wantToken    string
	}{
		{"dev defaults service token", map[string]string{"CP_DATABASE_URL": db}, false, "dev-service-token"},
		{"missing db", map[string]string{}, true, ""},
		{"prod requires token", map[string]string{"CP_DATABASE_URL": db, "CP_ENV": "prod"}, true, ""},
		{"prod with token ok", map[string]string{"CP_DATABASE_URL": db, "CP_ENV": "prod", "CP_SERVICE_TOKEN": "s3cret"}, false, "s3cret"},
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
