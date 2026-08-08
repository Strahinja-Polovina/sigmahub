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
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/githubapp"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/kms"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/paddle"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/reconciler"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
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

// runDeployDrain periodically drains queued deploy_requests into deployments and
// nudges the reconciler for each affected server, so a git push produces a
// rendered clone→build→rollout pipeline within a few seconds.
func runDeployDrain(ctx context.Context, log *slog.Logger, st *store.Store, rec *reconciler.Reconciler) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refs, err := st.DrainDeployRequests(ctx)
			if err != nil {
				log.Error("deploy drain", "err", err)
				continue
			}
			for _, r := range refs {
				rec.ReconcileAsync(r.OrgID, r.ServerID)
			}
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
	rec := reconciler.New(log, st, dsdKey)
	rec.SetACMEConfig(reconciler.ACMEConfig{Email: cfg.ACMEEmail, CADirURL: cfg.ACMECADirURL})
	go rec.Run(ctx, 60*time.Second)

	// Deploy-request drain (P1-9): turn queued git deploy_requests into
	// deployments and re-render the affected servers so the pipeline runs.
	go runDeployDrain(ctx, log, st, rec)

	// Backup scheduler (P1-11): the wall-clock primitive that turns policies
	// into due backup/verify runs and fails runs that stopped making progress.
	go backup.Run(ctx, log, st, rec, backup.Config{
		Interval: time.Minute,
		// Execution budget, from dispatch — just above the agent's 25m op cap.
		RunTimeout: 30 * time.Minute,
		// Queue budget, from enqueue — verify rows legitimately wait for their
		// backup's sha, and the agent applies ops serially (SIGMA-163).
		QueueTimeout: 6 * time.Hour,
	})

	// Alert dispatcher (P2-6): drains the alert outbox that state-change
	// producers fill, with retry/backoff per delivery.
	alertSender := alerts.NewSender()
	go alerts.Run(ctx, log, st, alertSender, alerts.Config{})

	// Background maintenance: flip silent servers to unreachable, prune old
	// metrics, and fail deployments whose agent stopped reporting. StaleAfter ≈
	// 3× the agent's default 30s heartbeat. DeployTimeout sits above the agent's
	// own per-op ceilings (a build plus a 120s health gate) so only a genuinely
	// dead apply is failed (SIGMA-182).
	go sweeper.Run(ctx, log, st, sweeper.Config{
		Interval:      30 * time.Second,
		StaleAfter:    90 * time.Second,
		Retention:     24 * time.Hour,
		DeployTimeout: 45 * time.Minute,
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
	// SIGMA-171: the billed quantity used to be sent to Paddle once, at
	// checkout, and never again — every org that grew or shrank after
	// subscribing was invoiced for its subscribe-time server count while the
	// dashboard showed the live figure. Nil client / no price id → no-op.
	quantitySync := &billingsync.Syncer{
		Store: st, Paddle: paddleQuantity, PriceID: cfg.PaddlePriceID, Log: log,
	}
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := st.SweepUsageHours(ctx, time.Now()); err != nil {
					log.Error("usage sweep", "err", err)
				}
				// P2-4: the time-integrated connected-server meter billing reads.
				if _, err := st.SweepServerHours(ctx, time.Now()); err != nil {
					log.Error("server-hours sweep", "err", err)
				}
				// SIGMA-65: enqueue a daily per-bucket storage measurement so the
				// object-storage meter stays current.
				if _, err := st.SweepS3Measure(ctx, time.Now()); err != nil {
					log.Error("s3 measure sweep", "err", err)
				}
				// SIGMA-171: turn the meter into an invoice — push the current
				// billable-server count to any subscription that has drifted.
				if n, err := quantitySync.Sync(ctx, time.Now()); err != nil {
					log.Error("billing quantity sync", "err", err)
				} else if n > 0 {
					log.Info("billing quantity synced", "subscriptions", n)
				}
			}
		}
	}()

	srv := &http.Server{
		Addr: cfg.Addr,
		Handler: api.New(log, st, st, st, api.Options{
			DevServiceToken:     cfg.ServiceToken,
			ProvisionToken:      cfg.ProvisionToken,
			Git:                 st,
			Inspector:           inspector,
			RepoLister:          inspector,
			InstallationTokens:  installTokens,
			InstallationAccounts: installAccounts,
			GitIntegration:       st,
			Compose:              st,
			Clusters:             st,
			LLM:                  st,
			DNS:                  st,
			GitHubAppSlug:        cfg.GitHubAppSlug,
			GitHubWebhookSecret: cfg.GitHubWebhookSecret,
			PublicURL:           cfg.PublicURL,
			DSDStore:            st,
			DSDWaiter:           rec,
			Reconcile:           rec,
			DSDPublicKey:        dsdKey.Public().(ed25519.PublicKey),
			Telemetry:           tel,
			TelemetryStore:      st,
			AlertSender:         alertSender,
			Billing:             st,
			Paddle:              paddleClient,
			PaddleWebhookSecret: cfg.PaddleWebhookSecret,
			PaddlePriceID:       cfg.PaddlePriceID,
			RequireActor:        cfg.RequireActor,
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
