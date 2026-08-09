// Package store owns the PostgreSQL connection pool and schema migrations.
package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/kms"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

type Store struct {
	Pool *pgxpool.Pool
	// pepper keys the HMAC used to hash tokens at rest (P0-9). It is loaded
	// from the KMS custody at boot via LoadTokenPepper/SetPepper.
	pepper []byte
	// custody wraps/unwraps per-org DEKs (P1-6). Set at boot via SetCustody.
	custody kms.KeyCustody
	// dekCache holds unwrapped org DEK plaintexts keyed by dek id, so a DEK is
	// custody-unwrapped (and audited) once per process, not per secret op.
	dekMu    sync.Mutex
	dekCache map[string][]byte
	// enabledDBEngines is the P1-10 engine allowlist (CP_DB_ENGINES). Nil means
	// all engines; the Postgres-only fallback build is this map with one entry.
	enabledDBEngines map[string]bool
	// enabledS3Engines is the P2-2 S3 engine allowlist (CP_S3_ENGINES). Nil
	// means all engines (minio, seaweedfs).
	enabledS3Engines map[string]bool
	// installTokens mints GitHub App installation tokens (SIGMA-55). Nil when
	// no App is configured — connections then rely on their stored PAT.
	installTokens InstallationTokenSource
	// modelSizer estimates a model's VRAM for the create-time fit check
	// (SIGMA-214). Nil — the default — means no fit check, which is also what
	// every failure of a configured sizer degrades to; see llm_fit.go.
	modelSizer ModelSizer
	// hubToken is CP_HUGGING_FACE_TOKEN, seeded into each inference endpoint's
	// weights credential at provision (SIGMA-213). Empty is a supported
	// configuration — public models need no credential — and is the honest
	// "no" behind WeightsTokenAvailable.
	hubToken string
}

// SetHuggingFaceToken installs the control plane's Hugging Face token, the one
// CreateResource seeds an inference endpoint's HUGGING_FACE_HUB_TOKEN from.
//
// It is the SAME token the model picker authenticates with, and that is the
// point of it being one value: the account that can see a gated repository in
// the wizard is the account that fetches its weights on the GPU host, so the
// wizard's promise and the download's outcome cannot disagree.
func (s *Store) SetHuggingFaceToken(token string) { s.hubToken = token }

// InstallationTokenSource mints short-lived GitHub App installation access
// tokens for connections that carry an installation id.
type InstallationTokenSource interface {
	InstallationToken(ctx context.Context, installationID string) (string, error)
}

// SetInstallationTokens installs the GitHub App token minter. Optional; the
// PAT path keeps working without it.
func (s *Store) SetInstallationTokens(src InstallationTokenSource) {
	s.installTokens = src
}

// SetCustody installs the key-custody boundary used to wrap/unwrap per-org DEKs.
// Must be called at boot before any secret operation.
func (s *Store) SetCustody(c kms.KeyCustody) {
	s.custody = c
	s.dekCache = map[string][]byte{}
}

func Open(ctx context.Context, databaseURL string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	// A reconcile holds one connection for its advisory lock (SIGMA-94/120) AND
	// needs several more for its reads+write, so the pgxpool default floor of
	// max(4, NumCPU) is too tight under concurrent reconciles. Raise the floor
	// (respecting an explicit higher pool_max_conns in the DSN).
	if cfg.MaxConns < 20 {
		cfg.MaxConns = 20
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return &Store{Pool: pool}, nil
}

func (s *Store) Close() { s.Pool.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.Pool.Ping(ctx) }

// Migrate applies embedded migrations in filename order. Each migration runs
// in a transaction and is recorded in schema_migrations, so re-running on
// boot is idempotent.
func (s *Store) Migrate(ctx context.Context, log *slog.Logger) error {
	if _, err := s.Pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			filename   TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	entries, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		return fmt.Errorf("list migrations: %w", err)
	}
	sort.Strings(entries)

	for _, name := range entries {
		var applied bool
		if err := s.Pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE filename = $1)`, name,
		).Scan(&applied); err != nil {
			return fmt.Errorf("check %s: %w", name, err)
		}
		if applied {
			continue
		}

		sql, err := migrationFS.ReadFile(name)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		tx, err := s.Pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (filename) VALUES ($1)`, name,
		); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("record %s: %w", name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit %s: %w", name, err)
		}
		log.Info("migration applied", "file", name)
	}
	return nil
}
