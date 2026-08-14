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
	"time"

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
	// clusterTokenCache memoises unwrapped cluster join tokens, keyed by cluster
	// id, for exactly the same reason dekCache exists (SIGMA-319).
	//
	// ClusterMembershipForServer is on the reconcile path, and the reconciler
	// re-renders every server every 60s, so an unconditional unwrap here is one
	// Vault transit round trip AND one cp_audit_log row per node per pass — tens
	// of thousands a day restating a token that never moved. The entry is
	// validated against the `join_token_wrapped` bytes the membership query
	// already read, so a token that IS replaced invalidates itself on the next
	// read without needing any explicit invalidation call.
	clusterTokenMu    sync.Mutex
	clusterTokenCache map[string]cachedClusterToken
	// enabledDBEngines is the P1-10 engine allowlist (CP_DB_ENGINES). Nil means
	// all engines; the Postgres-only fallback build is this map with one entry.
	enabledDBEngines map[string]bool
	// enabledS3Engines is the P2-2 S3 engine allowlist (CP_S3_ENGINES). Nil
	// means all engines (minio, seaweedfs).
	enabledS3Engines map[string]bool
	// installTokens mints GitHub App installation tokens (SIGMA-55). Nil when
	// no App is configured — connections then rely on their stored PAT.
	installTokens InstallationTokenSource
	// modelSizer looks a model up for the create-time checks — its VRAM, its
	// format, its task and its context ceiling (SIGMA-213, SIGMA-214). Nil — the
	// default — means no checks at all, which is also what every failure of a
	// configured sizer degrades to; see llm_fit.go.
	modelSizer ModelSizer
	// appsDomain is CP_APPS_DOMAIN — the wildcard the operator has pointed at
	// their proxy servers. Empty is fully supported and falls back to sslip.io;
	// see PublicHost.
	appsDomain string
	// billingConfigured is whether this deployment can actually take money —
	// Paddle credentials AND a price id. It gates the free-tier ceiling
	// (SIGMA-363) and nothing else.
	billingConfigured bool
}

// SetAppsDomain installs the wildcard domain SigmaHub mints resource URLs under
// (SIGMA-351). Empty means no wildcard is configured, which is not an error: the
// sslip.io fallback keeps a fresh install reachable on its first deploy.
func (s *Store) SetAppsDomain(domain string) { s.appsDomain = domain }

// SetBillingConfigured tells the store whether this deployment can take money:
// Paddle credentials and a price id, the same condition that decides whether the
// dashboard can open a checkout.
//
// It gates ONE thing — the free-tier ceiling (SIGMA-363) — and it has to, because
// a paywall on a deployment with no way to pay is not a paywall, it is a broken
// product. A self-hosted SigmaHub with no Paddle configured is the ordinary case
// and must keep growing without limit; the ceiling exists to stop a HOSTED tenant
// from using the paid tiers for free, and a hosted deployment is exactly the one
// that has these credentials. Default false, so every path that has not been told
// otherwise (tests, self-hosted, demo) is uncapped.
func (s *Store) SetBillingConfigured(v bool) { s.billingConfigured = v }

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
	// Both memos are custody-scoped: a new custody can decrypt different
	// envelopes, so anything unwrapped under the old one has to go.
	s.clusterTokenMu.Lock()
	s.clusterTokenCache = map[string]cachedClusterToken{}
	s.clusterTokenMu.Unlock()
}

// cachedClusterToken is one memoised join token plus the wrapped bytes it was
// derived from. Comparing the envelope is what makes the memo self-invalidating
// (SIGMA-319): the membership query reads join_token_wrapped anyway, so a
// replaced token is detected without an extra query and without any caller
// having to remember to purge.
type cachedClusterToken struct {
	wrapped []byte
	token   string
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

// migrateLockKey is the advisory-lock key every migration run takes
// (SIGMA-290). It is a NAME hashed by Postgres rather than a magic number so
// the web half — which migrates the same database with drizzle from
// web/src/server/db/migrate-prod.ts — can take the identical lock by writing
// the identical string. The two migration ledgers are separate; the exclusion
// between the processes touching one database is not.
const migrateLockKey = "sigmahub:migrate"

// Migrate applies embedded migrations in filename order. Each migration runs
// in a transaction and is recorded in schema_migrations, so re-running on
// boot is idempotent.
//
// The whole run is serialised behind a session-level advisory lock (SIGMA-290).
// Migration is a side effect of process start, so the number of processes
// racing it is the number of replicas, and the migrations are bare,
// non-idempotent DDL. Without the lock two replicas of the same release both
// see a file unapplied, one takes ACCESS EXCLUSIVE on the new object and the
// other fails with `relation "x" already exists` — Migrate returns an error,
// the process exits 1, the supervisor restarts it and the operator watches a
// crash-looping control plane. Even the CREATE TABLE IF NOT EXISTS below races:
// concurrent creates collide on pg_type's unique index, not on the IF NOT
// EXISTS check.
//
// BLOCKING is the correct behaviour, which is why this is pg_advisory_lock and
// not the try/skip loop LockServerReconcile uses: a replica that arrives second
// must wait for the first to finish and then find every migration recorded,
// rather than serve requests against a schema it has not seen applied. The lock
// is held on ONE checked-out connection for the whole run; the loop's own
// queries take other pool connections, so the pool must have at least two —
// Open floors it at 20.
func (s *Store) Migrate(ctx context.Context, log *slog.Logger) error {
	lockConn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration lock connection: %w", err)
	}
	defer lockConn.Release()
	if _, err := lockConn.Exec(ctx, `SELECT pg_advisory_lock(hashtext($1))`, migrateLockKey); err != nil {
		return fmt.Errorf("take migration lock: %w", err)
	}
	// Unlock explicitly rather than relying on the session ending: the
	// connection goes back to the pool, and a session-level lock left behind on
	// a pooled connection would wedge every later migration run against this
	// database, including the next boot of this same process.
	defer func() {
		uctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := lockConn.Exec(uctx, `SELECT pg_advisory_unlock(hashtext($1))`, migrateLockKey); err != nil {
			log.Warn("release migration lock", "err", err)
		}
	}()

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
