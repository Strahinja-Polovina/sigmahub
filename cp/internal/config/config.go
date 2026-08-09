// Package config resolves control-plane configuration from the environment.
package config

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
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
	// Unset enables every engine the catalog defines; "postgres" alone is the
	// pre-agreed M6 fallback build — a configuration cut, not a rewrite.
	DBEngines []string
	// S3Engines is the P2-2 object-storage engine allowlist (CP_S3_ENGINES).
	// Unset enables every engine the catalog defines; "minio" alone is the
	// MinIO-only build.
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
	// HuggingFaceToken (CP_HUGGING_FACE_TOKEN) is the Hugging Face account this
	// control plane acts as. It does BOTH jobs, and that is the whole point of
	// there being one setting: it authenticates the model picker's Hub calls in
	// this process, and it is seeded into each inference endpoint's
	// HUGGING_FACE_HUB_TOKEN secret so the agent DOWNLOADS the weights as the
	// same account (store.SetHuggingFaceToken). Those used to be two unrelated
	// things — the picker had a token, the download referenced a secret nothing
	// ever created — so the wizard could approve a gated model whose weights
	// then 401'd tens of gigabytes into a pull on a GPU-billed host.
	//
	// OPTIONAL, and deliberately not defaulted or required: the Hub's model API
	// serves public repos unauthenticated and public weights download without
	// credentials, so a self-hoster who sets nothing gets a working picker and
	// working deploys over the models most people run. A token adds the org's
	// private repos and the gated ones (Llama & co) it has been granted, and the
	// search response says whether the operator's target can actually fetch them
	// — see the API's tokenConfigured field, which reports the WEIGHTS
	// credential rather than this one, because that is the question the wizard
	// is really asking.
	HuggingFaceToken string
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
		HuggingFaceToken:        strings.TrimSpace(os.Getenv("CP_HUGGING_FACE_TOKEN")),
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

	// P1-10 engine allowlist, and P2-2's for object storage. Both ask the store
	// catalog what exists instead of holding a list — see parseEngineList.
	if cfg.DBEngines, err = parseEngineList("CP_DB_ENGINES", store.DBEngineKinds()); err != nil {
		return Config{}, err
	}
	if cfg.S3Engines, err = parseEngineList("CP_S3_ENGINES", store.S3EngineNames()); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// parseEngineList reads a comma-separated engine allowlist from key, defaulting
// to every engine in known and rejecting anything that is not one of them. A
// typo must fail boot, not silently disable an engine an operator believes is
// on.
//
// known is passed in from the store catalog rather than written down here, and
// the message RENDERS it rather than restating it. Both halves of that were
// wrong before SIGMA-216: this file kept a knownDBEngines map AND spelled the
// same four names again inside the error text AND a third time as the default
// value, so an engine added to the catalog left the map rejecting it, and an
// engine removed left the message advertising it. Nothing read the sentence, so
// nothing could notice it had gone stale — the operator who mistyped an engine
// name was handed a list of the engines this product supported on whatever day
// that string was last edited.
//
// Deriving it needs `config` to import `store`, which is not a cycle: store's
// own imports are kms, dsd, gitdetect and hf, none of which read configuration,
// and config is a leaf that only cmd/sigmahub-cp imports. The alternative —
// moving the check up to main.go, where store is already imported — would leave
// FromEnv returning a Config whose engine list had not been validated, and split
// "what CP_DB_ENGINES may say" from the function that parses it. That is the
// same two-places-for-one-fact shape as the map this replaced, so it is not the
// fix.
func parseEngineList(key string, known []string) ([]string, error) {
	var enabled []string
	for _, e := range strings.Split(getenv(key, strings.Join(known, ",")), ",") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if !slices.Contains(known, e) {
			return nil, fmt.Errorf("%s: unknown engine %q (known: %s)", key, e, strings.Join(known, ", "))
		}
		enabled = append(enabled, e)
	}
	if len(enabled) == 0 {
		return nil, fmt.Errorf("%s must enable at least one engine", key)
	}
	return enabled, nil
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
