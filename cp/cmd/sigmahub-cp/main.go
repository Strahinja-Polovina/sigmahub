// sigmahub-cp is the SigmaHub control plane API server.
package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/alerts"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/api"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/backup"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/billingsync"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/config"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/cpmetrics"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/githubapp"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/hf"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/kms"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/paddle"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/reconciler"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/supervise"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/sweeper"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/telemetry"
)

// version is stamped at release time via -ldflags "-X main.version=…".
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "mint-service-token" {
		if err := mintServiceToken(os.Args[2:]); err != nil {
			slog.Error("fatal", "err", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		if err := migrateOnly(); err != nil {
			slog.Error("fatal", "err", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// mintServiceToken is the operator-side bootstrap for dashboard credentials:
// it writes straight to the database, so no pre-existing token is needed.
func mintServiceToken(args []string) error {
	fs := flag.NewFlagSet("mint-service-token", flag.ExitOnError)
	org := fs.String("org", "", "organization id the token is scoped to (required)")
	roleFlag := fs.String("role", string(store.RoleDeveloper), `token role: "Org Admin", "Project Admin" or "Developer"`)
	name := fs.String("name", "", "token label, shown as the audit actor (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *org == "" || *name == "" {
		fs.Usage()
		return errors.New("both -org and -name are required")
	}
	role, err := store.ParseRole(*roleFlag)
	if err != nil {
		return err
	}

	databaseURL := os.Getenv("CP_DATABASE_URL")
	if databaseURL == "" {
		return errors.New("CP_DATABASE_URL is required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := setupStore(ctx, slog.Default(), databaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	tok, p, err := st.IssueServiceToken(ctx, *org, *name, role, "cli")
	if err != nil {
		return err
	}
	fmt.Printf("service token %s (org %s, role %s):\n\n  %s\n\nShown once — only its hash is stored.\n", p.ID, p.OrgID, p.Role, tok)
	return nil
}

// migrateOnly applies the schema and exits, so migration can be a DEPLOY STEP
// rather than only a side effect of process start (SIGMA-290).
//
// Until this existed, the only way to migrate was to boot something: the API
// server, or — surprisingly — `mint-service-token`, which goes through
// setupStore and therefore migrates a database as a side effect of issuing a
// token. That makes "when does the schema change" an answer nobody can state,
// and it is the reason `replicas: 2` used to mean two processes racing the DDL.
// Run this once against the new image before rolling any replicas; the
// advisory lock in Store.Migrate makes running it concurrently with a booting
// replica safe rather than merely unlikely.
//
// It deliberately does NOT load the KMS custody or the token pepper: applying
// schema must not require the production key material to be reachable from
// wherever the deploy step runs.
func migrateOnly() error {
	databaseURL := os.Getenv("CP_DATABASE_URL")
	if databaseURL == "" {
		return errors.New("CP_DATABASE_URL is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log := slog.Default()
	st, err := store.Open(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx, log); err != nil {
		return err
	}
	log.Info("migrations applied")
	return nil
}

// loadCustody constructs the key-custody boundary from CP_KMS_BACKEND:
//   - "file" (default): the dev-grade AES-GCM key file at CP_KMS_KEY_FILE —
//     envelope hygiene + audit trail, NOT a production trust boundary.
//   - "vault": HashiCorp Vault transit (CP_VAULT_ADDR, CP_VAULT_TOKEN,
//     CP_VAULT_TRANSIT_KEY, optional CP_VAULT_NAMESPACE) — the production
//     custody: master key material never touches this host, and Vault's own
//     audit log is the out-of-band anchor P0-9 calls for.
//
// Switching backends does NOT re-wrap existing envelopes: bootstrap a fresh
// deployment on vault, or re-wrap (KEK rotation covers org DEKs) before
// flipping an existing one.
func loadCustody(ctx context.Context, st *store.Store) (kms.KeyCustody, error) {
	switch backend := os.Getenv("CP_KMS_BACKEND"); backend {
	case "", "file":
		keyPath := os.Getenv("CP_KMS_KEY_FILE")
		if keyPath == "" {
			keyPath = filepath.Join(".data", "cp-kms.key")
		}
		return kms.LoadOrCreateFileCustody(keyPath, st.AuditUnwrapSink())
	case "vault":
		return kms.NewVaultCustody(ctx, kms.VaultConfig{
			Addr:       os.Getenv("CP_VAULT_ADDR"),
			Token:      os.Getenv("CP_VAULT_TOKEN"),
			TransitKey: os.Getenv("CP_VAULT_TRANSIT_KEY"),
			Namespace:  os.Getenv("CP_VAULT_NAMESPACE"),
		}, st.AuditUnwrapSink())
	default:
		return nil, fmt.Errorf(`CP_KMS_BACKEND must be "file" or "vault", got %q`, backend)
	}
}

// setupStore opens the DB, applies migrations, and installs the KMS-custodied
// token pepper so token hashing is keyed (P0-9). Every custody unwrap writes
// an audit row into cp_audit_log via the sink.
func setupStore(ctx context.Context, log *slog.Logger, databaseURL string) (*store.Store, error) {
	st, err := store.Open(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	if err := st.Migrate(ctx, log); err != nil {
		st.Close()
		return nil, err
	}
	custody, err := loadCustody(ctx, st)
	if err != nil {
		st.Close()
		return nil, err
	}
	pepper, err := st.LoadTokenPepper(ctx, custody)
	if err != nil {
		st.Close()
		return nil, err
	}
	st.SetPepper(pepper)
	// Same custody wraps/unwraps per-org secret DEKs (P1-6).
	st.SetCustody(custody)
	return st, nil
}

// loadDSDKey unwraps (or creates) the Ed25519 DSD-signing key through the same
// KMS custody used for the token pepper.
func loadDSDKey(ctx context.Context, st *store.Store) (ed25519.PrivateKey, error) {
	custody, err := loadCustody(ctx, st)
	if err != nil {
		return nil, err
	}
	return st.LoadDSDSigningKey(ctx, custody)
}

// loadGitHubApp wires the SIGMA-55 installation-token minter when a GitHub
// App is configured: the App private key rides the same KMS custody as the
// other CP secrets (imported from CP_GITHUB_APP_PRIVATE_KEY_FILE on first
// boot). Returns nil with no error when the App is simply not set up;
// half-configuration fails boot rather than silently minting nothing.
func loadGitHubApp(ctx context.Context, st *store.Store, cfg config.Config) (*githubapp.AppAuth, error) {
	custody, err := loadCustody(ctx, st)
	if err != nil {
		return nil, err
	}
	key, err := st.LoadGitHubAppKey(ctx, custody, cfg.GitHubAppPrivateKeyFile)
	if err != nil {
		return nil, err
	}
	if key == nil && cfg.GitHubAppID == "" {
		return nil, nil
	}
	if key == nil {
		return nil, fmt.Errorf("CP_GITHUB_APP_ID is set but no App private key is available (set CP_GITHUB_APP_PRIVATE_KEY_FILE once to import it)")
	}
	if cfg.GitHubAppID == "" {
		return nil, fmt.Errorf("a GitHub App private key is imported but CP_GITHUB_APP_ID is not set")
	}
	return githubapp.NewAppAuth(cfg.GitHubAppID, key), nil
}

// hubSizer adapts the Hugging Face client to store.ModelSizer.
//
// The adapter exists so the store never holds the card itself. Handing it the
// hf.Client directly would work and would be a mistake: the store would then be
// one autocomplete away from making a provisioning decision out of `gated`,
// `likes` or `sizingBasis`, which are the picker's business, and it would carry
// an HTTP client into a package whose job is transactions. What crosses this
// line is exactly what a CREATE decides on, and every field below has a refusal
// or a rendered flag behind it.
type hubSizer struct{ hub *hf.Client }

func (h hubSizer) SizeModel(ctx context.Context, repoID string) (store.ModelSize, error) {
	card, err := h.hub.Resolve(ctx, repoID)
	if err != nil {
		return store.ModelSize{}, err
	}
	return store.ModelSize{
		ParametersKnown:   card.ParametersKnown,
		VRAMBytesRequired: card.VRAMBytesRequired,
		// Carried, not re-rendered: the refusal must quote the same string the
		// picker put on screen for this model.
		VRAMText: card.VRAMText,
		// The two the wizard refuses at the model step, so an API-direct create
		// hits the same wall: vLLM cannot open a GGUF repository, and an `llm`
		// resource serves text generation and nothing else.
		Quantization: card.Quantization,
		PipelineTag:  card.PipelineTag,
		// The model's own context ceiling, which the endpoint's --max-model-len
		// is clamped to at provision. 0 is "the Hub did not say" and renders no
		// flag at all.
		MaxPositionEmbeddings: card.MaxPositionEmbeddings,
	}, nil
}

// runDeployDrain periodically drains queued deploy_requests into deployments and
// nudges the reconciler for each affected server, so a git push produces a
// rendered clone→build→rollout pipeline within a few seconds.
func runDeployDrain(ctx context.Context, log *slog.Logger, st *store.Store, rec *reconciler.Reconciler, beat *cpmetrics.Loop) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Supervised (SIGMA-250): a panic in one drain abandons that drain
			// rather than terminating the control plane for every tenant.
			beat.Report(supervise.Pass(log, "deploy_drain", func() error {
				refs, err := st.DrainDeployRequests(ctx)
				if err != nil {
					log.Error("deploy drain", "err", err)
					return err
				}
				for _, r := range refs {
					rec.ReconcileAsync(r.OrgID, r.ServerID)
				}
				return nil
			}))
		}
	}
}

func run() error {
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}

	var log *slog.Logger
	if cfg.Env == "prod" {
		log = slog.New(slog.NewJSONHandler(os.Stdout, nil))
	} else {
		log = slog.New(slog.NewTextHandler(os.Stdout, nil))
	}
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := setupStore(ctx, log, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()
	// P1-10 engine allowlist (the Postgres-only fallback build is CP_DB_ENGINES=postgres).
	st.SetEnabledDBEngines(cfg.DBEngines)
	// P2-2 object-storage engine allowlist (MinIO-only build is CP_S3_ENGINES=minio).
	st.SetEnabledS3Engines(cfg.S3Engines)

	// DSD signing key (custody-wrapped, stable across restarts) + reconciler.
	dsdKey, err := loadDSDKey(ctx, st)
	if err != nil {
		return err
	}

	// GitHub App installation-token minting (SIGMA-55). Optional: nil keeps
	// the PAT-only path. The typed-nil guard matters — a nil *AppAuth must not
	// become a non-nil interface.
	appAuth, err := loadGitHubApp(ctx, st, cfg)
	if err != nil {
		return err
	}
	var installTokens api.InstallationTokenSource
	// Kept as a separate interface variable rather than assigning appAuth
	// directly: a nil *AppAuth stored in an interface is NOT a nil interface, so
	// the "is the App configured?" checks in the API would all read true.
	var installAccounts api.InstallationAccountSource
	if appAuth != nil {
		st.SetInstallationTokens(appAuth)
		installTokens = appAuth
		installAccounts = appAuth
		log.Info("github app configured", "appId", cfg.GitHubAppID, "slug", cfg.GitHubAppSlug)
	}
	inspector := githubapp.NewInspector()

	// Hugging Face model catalog (SIGMA-213/214). Constructed UNCONDITIONALLY,
	// token or no token, because the Hub's model API serves public repos
	// unauthenticated and the alternative — wiring it only when
	// CP_HUGGING_FACE_TOKEN is set — would hand a self-hoster an empty picker
	// and no way to tell that from "the Hub is down". The token, when present,
	// widens what the picker can see; it is not what makes the picker exist.
	//
	// The TTL is the picker's whole rate-limit story: a typeahead is many
	// keystrokes over a few models, and ten minutes is long enough that a wizard
	// session costs a handful of Hub calls while short enough that a repo which
	// gains weights, a licence gate or a new revision is current well within the
	// time it takes anyone to notice.
	hubClient := &hf.Client{
		HTTP:  &http.Client{Timeout: 10 * time.Second},
		Token: cfg.HuggingFaceToken,
		TTL:   10 * time.Minute,
	}
	// The store's create-time fit re-check reads the same client through a
	// two-field interface, so an API-direct create hits the wall the wizard
	// draws. Every failure of this call degrades to "no check" (llm_fit.go).
	st.SetModelSizer(hubSizer{hub: hubClient})
	// The SAME token, and that is the fix rather than an implementation detail:
	// it authenticates the PICKER's metadata lookups in this process and goes no
	// further. It used to be seeded into every tenant's inference endpoint as
	// their weights credential too, which put one operator-owned Hub account
	// into containers on hosts the customers own and can read (SIGMA-302). A
	// tenant that needs gated weights supplies their own HUGGING_FACE_HUB_TOKEN
	// project secret, and WeightsTokenAvailable reports on THAT.
	log.Info("model catalog configured", "hubTokenConfigured", hubClient.TokenConfigured())
	// The wildcard resource URLs are minted under, if the operator set one. Empty
	// is supported and falls back to sslip.io per host (SIGMA-351).
	st.SetAppsDomain(cfg.AppsDomain)
	log.Info("resource URLs configured", "appsDomain", cfg.AppsDomain)

	// The control plane's report on itself (SIGMA-248). Every background loop
	// below reports the outcome of each pass through this registry, and GET
	// /metrics exposes the last-success timestamps — so a loop that is erroring
	// on every tick stops being indistinguishable from one that is working.
	// Registered up front, before any `go`, so a loop that never starts is
	// reported as "never succeeded" rather than being absent.
	metrics := cpmetrics.New()
	metrics.SetPoolSource(func() cpmetrics.PoolStats {
		s := st.Pool.Stat()
		return cpmetrics.PoolStats{
			Acquired: s.AcquiredConns(), Idle: s.IdleConns(),
			Total: s.TotalConns(), Max: s.MaxConns(),
		}
	})

	rec := reconciler.New(log, st, dsdKey)
	rec.SetACMEConfig(reconciler.ACMEConfig{Email: cfg.ACMEEmail, CADirURL: cfg.ACMECADirURL})
	// SIGMA-262: agent.update ops point the agent at this control plane's own
	// /dl proxy, so a fleet on a private release repo can be upgraded.
	rec.SetPublicURL(cfg.PublicURL)
	rec.SetObservers(metrics.Loop(cpmetrics.LoopReconcilerResync).Report, metrics.ObserveDSDRender)
	// SIGMA-320: how long a whole fleet pass takes, so the 60s drift-repair SLO
	// degrading as the fleet grows is visible before a customer reports it.
	rec.SetResyncPassObserver(metrics.ObserveResyncPass)
	// Cross-replica long-poll wake-ups over Postgres LISTEN/NOTIFY (SIGMA-291):
	// without this the waiter map is per-process, so an agent long-polling one
	// replica sleeps out its whole window when another replica renders its
	// change. Harmless with a single instance — the publish is a no-op fan-out
	// back to this same listener.
	rec.SetChangeBus(st)
	go st.SubscribeDSDChanges(ctx, log, rec.WakeServer)
	go rec.Run(ctx, 60*time.Second)

	// Deploy-request drain (P1-9): turn queued git deploy_requests into
	// deployments and re-render the affected servers so the pipeline runs.
	go runDeployDrain(ctx, log, st, rec, metrics.Loop(cpmetrics.LoopDeployDrain))

	// Backup scheduler (P1-11): the wall-clock primitive that turns policies
	// into due backup/verify runs and fails runs that stopped making progress.
	go backup.Run(ctx, log, st, rec, backup.Config{
		Interval: time.Minute,
		// Execution budget, from dispatch — just above the agent's 25m op cap.
		RunTimeout: 30 * time.Minute,
		// Queue budget, from enqueue — verify rows legitimately wait for their
		// backup's sha, and the agent applies ops serially (SIGMA-163).
		QueueTimeout: 6 * time.Hour,
		Heartbeat:    metrics.Loop(cpmetrics.LoopBackupScheduler).Report,
	})

	// Alert dispatcher (P2-6): drains the alert outbox that state-change
	// producers fill, with retry/backoff per delivery.
	alertSender := alerts.NewSender()
	go alerts.Run(ctx, log, st, alertSender, alerts.Config{
		Heartbeat: metrics.Loop(cpmetrics.LoopAlertDispatcher).Report,
	})

	// Background maintenance: flip silent servers to unreachable, prune old
	// metrics, and fail deployments whose agent stopped reporting. StaleAfter ≈
	// 3× the agent's default 30s heartbeat. DeployTimeout sits above the agent's
	// own per-op ceilings (a build plus a 120s health gate) so only a genuinely
	// dead apply is failed (SIGMA-182).
	// DecommissionTimeout is the other half of "the CP completes on ack OR a
	// timeout" (SIGMA-204): ten minutes is long enough for a real teardown
	// (container stop grace periods, a long-poll cycle, an apt-busy host) and
	// short enough that an operator watching the row does not conclude the
	// product hung.
	go sweeper.Run(ctx, log, st, sweeper.Config{
		Interval:   30 * time.Second,
		StaleAfter: 90 * time.Second,
		// Retention is CP_METRICS_RETENTION (24h by default) and is handed to
		// the API too — what this sweeper deletes is exactly how far back the
		// metrics endpoint can honestly serve when no pipeline is configured
		// (SIGMA-257), so the two must never be separate literals again.
		Retention:           cfg.MetricsRetention,
		DeployTimeout:       45 * time.Minute,
		DecommissionTimeout: 10 * time.Minute,
		Heartbeat:           metrics.Loop(cpmetrics.LoopSweeper).Report,
		// Retention for the append-only growth tables (SIGMA-249). These are the
		// product's defaults, and each is a decision rather than a round number:
		//
		//   - deploy_logs, 30 days after the deployment FINISHED. It is by far the
		//     largest table (one row per streamed build-log line) and the least
		//     read: the UI streams an in-flight build and shows the last lines of
		//     recent ones. A month covers "what did last sprint's failing deploy
		//     say"; a year of it is tens of gigabytes nobody opens.
		//   - cp_audit_log, 400 days — deliberately just over a year so an annual
		//     review can look back at the same month last year. This is the entry
		//     with a compliance dimension, so it is chosen explicitly and long
		//     rather than defaulted short.
		//   - deploy_requests, 30 days, drained rows only.
		//   - webhook_deliveries, 30 days. It only exists to make a REDELIVERED
		//     webhook a no-op, and providers retry for hours, so this is already
		//     two orders of magnitude past useful.
		//   - alert_outbox, 90 days, finalized rows only — long enough that "did
		//     we ever page anyone about this?" is answerable for a quarter.
		//   - idempotency_keys, 7 days. This one is a PROMISE, not just storage:
		//     the Idempotency-Key header's whole contract is "a retry within this
		//     long will not execute twice", so the window wants to be comfortably
		//     longer than any client's retry loop and no longer. A client retry
		//     arrives in seconds; a week is the generous end of that.
		Retain: store.Retention{
			DeployLogs:        30 * 24 * time.Hour,
			Audit:             400 * 24 * time.Hour,
			DeployRequests:    30 * 24 * time.Hour,
			WebhookDeliveries: 30 * 24 * time.Hour,
			AlertOutbox:       90 * 24 * time.Hour,
			IdempotencyKeys:   7 * 24 * time.Hour,
		},
	})

	// Telemetry forwarder (P1-13) + the hourly idempotent usage aggregates
	// (the A-4 metering hook Phase 2 billing reads).
	tel := telemetry.New(telemetry.Config{
		VMWriteURL: cfg.VMWriteURL,
		VMReadURL:  cfg.VMReadURL,
		LokiURL:    cfg.LokiURL,
	})
	// Paddle billing (P2-4): nil client when no API key → billing endpoints
	// answer honest not-configured. The typed-nil guard matters — a nil
	// *paddle.Client must not become a non-nil interface.
	var paddleClient api.PaddleClient
	// Held separately (and assigned from the same concrete client) so a nil
	// *paddle.Client never becomes a non-nil interface on either side.
	var paddleQuantity billingsync.PaddleClient
	if pc := paddle.NewClient(cfg.PaddleEnv, cfg.PaddleAPIKey); pc != nil {
		paddleClient, paddleQuantity = pc, pc
		log.Info("paddle billing configured", "env", cfg.PaddleEnv)
	}
	// SIGMA-363: the free tier has a ceiling, but ONLY where there is a way to
	// pay. Both halves are required — a client with no price id can take no
	// checkout — and a deployment missing either keeps growing without limit,
	// which is the ordinary self-hosted case.
	st.SetBillingConfigured(paddleClient != nil && cfg.PaddlePriceID != "")
	// SIGMA-171: the billed quantity used to be sent to Paddle once, at
	// checkout, and never again — every org that grew or shrank after
	// subscribing was invoiced for its subscribe-time server count while the
	// dashboard showed the live figure. Nil client / no price id → no-op.
	quantitySync := &billingsync.Syncer{
		Store: st, Paddle: paddleQuantity, PriceID: cfg.PaddlePriceID, Log: log,
	}
	usageBeat := metrics.Loop(cpmetrics.LoopUsageSweep)
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Supervised (SIGMA-250): a panic here abandons this pass rather
				// than terminating the control plane for every tenant. The pass's
				// verdict for the heartbeat is the FIRST step that failed — all
				// four meter or bill, so a pass that ran three of them is a pass
				// that under-reported somebody's usage.
				usageBeat.Report(supervise.Pass(log, "usage_sweep", func() error {
					var passErr error
					fail := func(err error) {
						if passErr == nil {
							passErr = err
						}
					}
					if _, err := st.SweepUsageHours(ctx, time.Now()); err != nil {
						log.Error("usage sweep", "err", err)
						fail(err)
					}
					// P2-4: the time-integrated connected-server meter billing reads.
					if _, err := st.SweepServerHours(ctx, time.Now()); err != nil {
						log.Error("server-hours sweep", "err", err)
						fail(err)
					}
					// SIGMA-65: enqueue a daily per-bucket storage measurement so the
					// object-storage meter stays current.
					if _, err := st.SweepS3Measure(ctx, time.Now()); err != nil {
						log.Error("s3 measure sweep", "err", err)
						fail(err)
					}
					// SIGMA-171: turn the meter into an invoice — push the current
					// billable-server count to any subscription that has drifted.
					if n, err := quantitySync.Sync(ctx, time.Now()); err != nil {
						log.Error("billing quantity sync", "err", err)
						fail(err)
					} else if n > 0 {
						log.Info("billing quantity synced", "subscriptions", n)
					}
					// SIGMA-295: chase delinquent orgs on a schedule. Warn rather
					// than Info — alert channels are per-org, so this log line is
					// the operator's only notice that a tenant is not paying.
					if n, err := st.SweepBillingDunning(ctx, time.Now()); err != nil {
						log.Error("billing dunning sweep", "err", err)
						fail(err)
					} else if n > 0 {
						log.Warn("billing dunning: delinquent orgs reminded", "orgs", n)
					}
					// SIGMA-363: orgs over the free tier with no live subscription.
					// They have no Paddle relationship to reconcile against, so
					// before this they appeared in no operator view at all — the
					// one shape of unpaid usage that was completely invisible.
					// Growth is already refused at server creation; this is how a
					// human finds out it is happening. Warn, for the same reason
					// dunning warns: alert channels are per-org, so this line is
					// the operator's only notice.
					if unbilled, err := st.UnbilledOrgs(ctx, time.Now()); err != nil {
						log.Error("unbilled usage report", "err", err)
						fail(err)
					} else if len(unbilled) > 0 {
						value := 0
						for _, o := range unbilled {
							value += o.MonthlyValue
						}
						log.Warn("unbilled usage: orgs over the free tier with no subscription",
							"orgs", len(unbilled), "monthly_value", value,
							"currency", store.BillingCurrency, "largest", unbilled[0].OrgID)
					}
					return passErr
				}))
			}
		}
	}()

	// Which release the installer routes serve. Unset means "the one this
	// binary was built from": .goreleaser.yaml builds sigmahub-cp and sigmad
	// from a single tag and stamps both with it, so a released control plane is
	// already pinned to a matching agent and needs no configuration. A source
	// build stamps "dev", which is not a release tag — GET /install.sh then
	// answers 503 naming CP_AGENT_VERSION rather than fetching a tag that does
	// not exist.
	agentVersion := cfg.AgentVersion
	if agentVersion == "" {
		agentVersion = version
	}
	// The credential itself is never logged, here or anywhere else — only
	// whether one is configured, which is the question an operator debugging a
	// 404 from a private repository actually has.
	log.Info("agent installer served from release repository",
		"repo", cfg.ReleaseRepo, "version", agentVersion, "authenticated", cfg.ReleaseToken != "")

	srv := &http.Server{
		Addr: cfg.Addr,
		Handler: api.New(log, st, st, st, api.Options{
			DevServiceToken:      cfg.ServiceToken,
			ProvisionToken:       cfg.ProvisionToken,
			Git:                  st,
			Inspector:            inspector,
			RepoLister:           inspector,
			InstallationTokens:   installTokens,
			InstallationAccounts: installAccounts,
			GitIntegration:       st,
			Compose:              st,
			Clusters:             st,
			Registry:             st,
			LLM:                  st,
			Models:               hubClient,
			DNS:                  st,
			GitHubAppSlug:        cfg.GitHubAppSlug,
			GitHubWebhookSecret:  cfg.GitHubWebhookSecret,
			PublicURL:            cfg.PublicURL,
			DBEngines:            cfg.DBEngines,
			S3Engines:            cfg.S3Engines,
			DSDStore:             st,
			DSDWaiter:            rec,
			Reconcile:            rec,
			DSDPublicKey:         dsdKey.Public().(ed25519.PublicKey),
			Telemetry:            tel,
			TelemetryStore:       st,
			OrgAdmin:             st, // tenant offboarding (SIGMA-298)
			MetricsRetention:     cfg.MetricsRetention,
			AlertSender:          alertSender,
			Billing:              st,
			Paddle:               paddleClient,
			PaddleWebhookSecret:  cfg.PaddleWebhookSecret,
			PaddlePriceID:        cfg.PaddlePriceID,
			RequireActor:         cfg.RequireActor,
			Release: api.ReleaseSource{
				Repo:    cfg.ReleaseRepo,
				Version: agentVersion,
				Token:   cfg.ReleaseToken,
			},
			Metrics: metrics,
		}).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("control plane listening", "addr", cfg.Addr, "version", version)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
