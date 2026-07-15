// sigmahub-cp is the SigmaHub control plane API server.
package main

import (
	"context"
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

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/api"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/config"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/kms"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/sweeper"
)

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

// setupStore opens the DB, applies migrations, and installs the KMS-custodied
// token pepper so token hashing is keyed (P0-9). The dev custody is a
// file-anchored AES-GCM key at CP_KMS_KEY_FILE (default ./.data/cp-kms.key);
// its unwrap of the pepper writes one audit row into cp_audit_log.
func setupStore(ctx context.Context, log *slog.Logger, databaseURL string) (*store.Store, error) {
	st, err := store.Open(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	if err := st.Migrate(ctx, log); err != nil {
		st.Close()
		return nil, err
	}
	keyPath := os.Getenv("CP_KMS_KEY_FILE")
	if keyPath == "" {
		keyPath = filepath.Join(".data", "cp-kms.key")
	}
	custody, err := kms.LoadOrCreateFileCustody(keyPath, st.AuditUnwrapSink())
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
	return st, nil
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

	// Background maintenance: flip silent servers to unreachable, prune old
	// metrics. StaleAfter ≈ 3× the agent's default 30s heartbeat.
	go sweeper.Run(ctx, log, st, sweeper.Config{
		Interval:   30 * time.Second,
		StaleAfter: 90 * time.Second,
		Retention:  24 * time.Hour,
	})

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           api.New(log, st, st, cfg.ServiceToken).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("control plane listening", "addr", cfg.Addr)
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
