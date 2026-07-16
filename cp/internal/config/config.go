// Package config resolves control-plane configuration from the environment.
package config

import (
	"fmt"
	"os"
)

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
	// ACMEEmail is the Let's Encrypt account contact rendered into proxy.traefik
	// ops (P1-8). ACMECADirURL overrides the CA directory — set to the Pebble /
	// LE-staging URL for e2e; empty means Let's Encrypt production.
	ACMEEmail    string
	ACMECADirURL string
}

func FromEnv() (Config, error) {
	cfg := Config{
		Addr:                getenv("CP_ADDR", ":8080"),
		DatabaseURL:         os.Getenv("CP_DATABASE_URL"),
		Env:                 getenv("CP_ENV", "dev"),
		ServiceToken:        os.Getenv("CP_SERVICE_TOKEN"),
		ProvisionToken:      os.Getenv("CP_PROVISION_TOKEN"),
		GitHubWebhookSecret: os.Getenv("CP_GITHUB_WEBHOOK_SECRET"),
		ACMEEmail:           os.Getenv("CP_ACME_EMAIL"),
		ACMECADirURL:        os.Getenv("CP_ACME_CA_DIR_URL"),
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
	return cfg, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
