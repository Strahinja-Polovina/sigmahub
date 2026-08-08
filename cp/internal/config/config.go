// Package config resolves control-plane configuration from the environment.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// knownDBEngines validates CP_DB_ENGINES entries; a typo must fail boot, not
// silently disable an engine.
var knownDBEngines = map[string]bool{"postgres": true, "mysql": true, "redis": true, "mongodb": true}

// knownS3Engines validates CP_S3_ENGINES entries (P2-2), same fail-loud rule.
var knownS3Engines = map[string]bool{"minio": true, "seaweedfs": true}

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
	// PublicURL is the CP's own public base URL; with GitHubWebhookSecret set,
	// connecting a repo auto-registers its push webhook against it.
	PublicURL string
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
	// S3Engines is the P2-2 object-storage engine allowlist (CP_S3_ENGINES).
	// Defaults to minio,seaweedfs; "minio" alone is the MinIO-only build.
	S3Engines []string
	// Telemetry sinks (P1-13). VMWriteURL/VMReadURL point at the
	// VictoriaMetrics cluster's vminsert/vmselect; LokiURL at Loki. Empty
	// disables that half of the pipeline — ingest is acknowledged-and-dropped
	// with a reason and the dashboards show an explicit not-configured state.
	VMWriteURL string
	VMReadURL  string
	LokiURL    string
	// Paddle billing (P2-4). PaddleAPIKey empty = billing off (no checkout /
	// no subscription sync); PaddleWebhookSecret empty = the webhook receiver
	// 503s rather than accept unverifiable deliveries (mirrors the GitHub
	// receiver). PaddleEnv selects sandbox vs production API base. PaddlePriceID
	// is the connected-server price the checkout/subscription bill against.
	PaddleAPIKey        string
	PaddleWebhookSecret string
	PaddleEnv           string
	PaddlePriceID       string
	// RequireActor (CP_REQUIRE_ACTOR=true) makes a valid X-Sigmahub-Actor header
	// mandatory on org-scoped service tokens (SIGMA-82). Off by default: the
	// actor header only ever NARROWS a token's role and is self-signed by the
	// token holder, so it is convenience, not a trust boundary — but an operator
	// who provisions per-role tokens can turn this on to fail closed when a
	// user-facing token is used with no actor. The dev wildcard token is exempt
	// (it is a system bypass with no per-user identity).
	RequireActor bool
}

func FromEnv() (Config, error) {
	cfg := Config{
		Addr:                    getenv("CP_ADDR", ":8080"),
		DatabaseURL:             os.Getenv("CP_DATABASE_URL"),
		Env:                     getenv("CP_ENV", "dev"),
		ServiceToken:            os.Getenv("CP_SERVICE_TOKEN"),
		ProvisionToken:          os.Getenv("CP_PROVISION_TOKEN"),
		PublicURL:               os.Getenv("CP_PUBLIC_URL"),
		GitHubWebhookSecret:     os.Getenv("CP_GITHUB_WEBHOOK_SECRET"),
		GitHubAppID:             os.Getenv("CP_GITHUB_APP_ID"),
		GitHubAppPrivateKeyFile: os.Getenv("CP_GITHUB_APP_PRIVATE_KEY_FILE"),
		GitHubAppSlug:           os.Getenv("CP_GITHUB_APP_SLUG"),
		ACMEEmail:               os.Getenv("CP_ACME_EMAIL"),
		ACMECADirURL:            os.Getenv("CP_ACME_CA_DIR_URL"),
		VMWriteURL:              os.Getenv("CP_VM_WRITE_URL"),
		VMReadURL:               os.Getenv("CP_VM_READ_URL"),
		LokiURL:                 os.Getenv("CP_LOKI_URL"),
		PaddleAPIKey:            os.Getenv("CP_PADDLE_API_KEY"),
		PaddleWebhookSecret:     os.Getenv("CP_PADDLE_WEBHOOK_SECRET"),
		PaddleEnv:               getenv("CP_PADDLE_ENV", "sandbox"),
		PaddlePriceID:           os.Getenv("CP_PADDLE_PRICE_ID"),
	}
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("CP_DATABASE_URL is required")
	}
	// Parse CP_REQUIRE_ACTOR strictly. The old `== "true"` check left every
	// other truthy spelling ("1", "True", "TRUE") silently false, so a
	// misconfigured operator ran with the SIGMA-82 strict-mode security control
	// off while believing it was on (fail-open). Mirror the fail-loud contract
	// the rest of this function enforces: unknown values fail boot.
	requireActor, err := parseBoolEnv("CP_REQUIRE_ACTOR", false)
	if err != nil {
		return Config{}, err
	}
	cfg.RequireActor = requireActor
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
	// Paddle env must be sandbox|production; a typo must not silently point
	// live billing at the wrong API base.
	switch cfg.PaddleEnv {
	case "sandbox", "production":
	default:
		return Config{}, fmt.Errorf(`CP_PADDLE_ENV must be "sandbox" or "production", got %q`, cfg.PaddleEnv)
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

	// P2-2 S3 engine allowlist. Empty = both engines enabled.
	rawS3 := getenv("CP_S3_ENGINES", "minio,seaweedfs")
	for _, e := range strings.Split(rawS3, ",") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if !knownS3Engines[e] {
			return Config{}, fmt.Errorf("CP_S3_ENGINES: unknown engine %q (known: minio, seaweedfs)", e)
		}
		cfg.S3Engines = append(cfg.S3Engines, e)
	}
	if len(cfg.S3Engines) == 0 {
		return Config{}, fmt.Errorf("CP_S3_ENGINES must enable at least one engine")
	}
	return cfg, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// parseBoolEnv reads a boolean env var, returning def when unset/empty and an
// error on any value strconv.ParseBool rejects. Security-relevant flags use
// this so a typo fails boot instead of silently disabling the control.
func parseBoolEnv(key string, def bool) (bool, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean (true/false), got %q", key, os.Getenv(key))
	}
	return b, nil
}
