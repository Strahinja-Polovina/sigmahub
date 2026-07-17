// Package config resolves control-plane configuration from the environment.
package config

import (
	"fmt"
	"os"
	"strings"
)

// knownDBEngines validates CP_DB_ENGINES entries; a typo must fail boot, not
// silently disable an engine.
var knownDBEngines = map[string]bool{"postgres": true, "mysql": true, "redis": true, "mongodb": true}

type Config struct {
	// Addr is the HTTP listen address, e.g. ":8080".
	Addr string
	// DatabaseURL is the PostgreSQL connection string.
	DatabaseURL string
	// Env is "dev" or "prod"; switches log format.
	Env string
	// ServiceToken is the dev-only static bypass for dashboard-facing
	// endpoints (accepted as a wildcard Org Admin). Forbidden in prod, where
	// org-scoped tokens are minted via `sigmahub-cp mint-service-token`.
	ServiceToken string
	// ProvisionToken gates POST /v1/orgs (org provisioning: mints the
	// org-scoped web credential). Defaults in dev; required in prod for the
	// provisioning endpoint to be usable.
	ProvisionToken string
	// GitHubWebhookSecret verifies inbound GitHub webhook deliveries
	// (X-Hub-Signature-256 HMAC-SHA256). Empty disables the webhook receiver
	// (returns 503) rather than accepting unverifiable deliveries.
	GitHubWebhookSecret string
	// GitHub App (SIGMA-55): AppID + the downloaded private-key PEM enable
	// installation-token minting (the key is imported into KMS custody on
	// boot; the file can be removed afterwards). AppSlug builds the
	// dashboard's github.com/apps/<slug>/installations/new install link.
	// All empty = App integration off; PATs keep working.
	GitHubAppID             string
	GitHubAppPrivateKeyFile string
	GitHubAppSlug           string
	// ACMEEmail is the Let's Encrypt account contact rendered into proxy.traefik
	// ops (P1-8). ACMECADirURL overrides the CA directory — set to the Pebble /
	// LE-staging URL for e2e; empty means Let's Encrypt production.
	ACMEEmail    string
	ACMECADirURL string
	// DBEngines is the P1-10 engine allowlist (CP_DB_ENGINES, comma-separated).
	// Defaults to all four engines; "postgres" alone is the pre-agreed M6
	// fallback build — a configuration cut, not a rewrite.
	DBEngines []string
	// Telemetry sinks (P1-13). VMWriteURL/VMReadURL point at the
	// VictoriaMetrics cluster's vminsert/vmselect; LokiURL at Loki. Empty
	// disables that half of the pipeline — ingest is acknowledged-and-dropped
	// with a reason and the dashboards show an explicit not-configured state.
	VMWriteURL string
	VMReadURL  string
	LokiURL    string
}

func FromEnv() (Config, error) {
	cfg := Config{
		Addr:                    getenv("CP_ADDR", ":8080"),
		DatabaseURL:             os.Getenv("CP_DATABASE_URL"),
		Env:                     getenv("CP_ENV", "dev"),
		ServiceToken:            os.Getenv("CP_SERVICE_TOKEN"),
		ProvisionToken:          os.Getenv("CP_PROVISION_TOKEN"),
		GitHubWebhookSecret:     os.Getenv("CP_GITHUB_WEBHOOK_SECRET"),
		GitHubAppID:             os.Getenv("CP_GITHUB_APP_ID"),
		GitHubAppPrivateKeyFile: os.Getenv("CP_GITHUB_APP_PRIVATE_KEY_FILE"),
		GitHubAppSlug:           os.Getenv("CP_GITHUB_APP_SLUG"),
		ACMEEmail:               os.Getenv("CP_ACME_EMAIL"),
		ACMECADirURL:            os.Getenv("CP_ACME_CA_DIR_URL"),
		VMWriteURL:              os.Getenv("CP_VM_WRITE_URL"),
		VMReadURL:               os.Getenv("CP_VM_READ_URL"),
		LokiURL:                 os.Getenv("CP_LOKI_URL"),
	}
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("CP_DATABASE_URL is required")
	}
	// Validate Env explicitly: a typo (e.g. "production") must not silently
	// fall through to the dev defaults below (fail-open on auth).
	switch cfg.Env {
	case "dev", "prod":
	default:
		return Config{}, fmt.Errorf(`CP_ENV must be "dev" or "prod", got %q`, cfg.Env)
	}
	// The static token is a dev convenience with org-wildcard admin power; in
	// prod it must not exist so every caller is org-scoped and role-checked.
	if cfg.Env == "prod" && cfg.ServiceToken != "" {
		return Config{}, fmt.Errorf("CP_SERVICE_TOKEN is dev-only; mint org-scoped tokens with `sigmahub-cp mint-service-token` instead")
	}
	if cfg.Env == "dev" && cfg.ServiceToken == "" {
		cfg.ServiceToken = "dev-service-token"
	}
	if cfg.Env == "dev" && cfg.ProvisionToken == "" {
		cfg.ProvisionToken = "dev-provision-token"
	}
	// P1-10 engine allowlist. Empty = all engines enabled.
	raw := getenv("CP_DB_ENGINES", "postgres,mysql,redis,mongodb")
	for _, e := range strings.Split(raw, ",") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if !knownDBEngines[e] {
			return Config{}, fmt.Errorf("CP_DB_ENGINES: unknown engine %q (known: postgres, mysql, redis, mongodb)", e)
		}
		cfg.DBEngines = append(cfg.DBEngines, e)
	}
	if len(cfg.DBEngines) == 0 {
		return Config{}, fmt.Errorf("CP_DB_ENGINES must enable at least one engine")
	}
	return cfg, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
