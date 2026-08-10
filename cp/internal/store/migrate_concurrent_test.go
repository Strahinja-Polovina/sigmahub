package store

// SIGMA-290: two control planes booting at once must not fight over the schema.
//
// Migrate is a side effect of process start, so the number of processes racing
// it is the number of replicas — and `replicas: 2` is the obvious first move
// for anyone running this as a paid product. Nothing serialised the runs: each
// process asked `SELECT EXISTS` per file and applied the ones it did not see,
// and the CP's migrations are bare, non-idempotent DDL (CREATE TABLE
// alert_channels, ALTER TABLE ... ADD COLUMN). Both replicas see the same file
// unapplied, one takes ACCESS EXCLUSIVE on the new object, the other blocks and
// then fails with `relation "x" already exists`. Migrate returns an error, the
// process exits 1, `restart: unless-stopped` retries, and the operator watches
// a crash-looping control plane.
//
// This runs N Migrate calls concurrently against a database of its own (created
// and dropped by the test, so it cannot disturb the shared integration
// database) and asserts every one returns nil. Blocking is the correct
// behaviour here: a replica that arrives second should WAIT for the first to
// finish, then find every migration already recorded and apply nothing.

import (
	"context"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// freshDatabase creates a throwaway database next to CP_TEST_DATABASE_URL and
// returns a DSN pointing at it. Dropped on cleanup.
func freshDatabase(t *testing.T, name string) string {
	t.Helper()
	base := os.Getenv("CP_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("CP_TEST_DATABASE_URL not set; skipping migration race test")
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse CP_TEST_DATABASE_URL: %v", err)
	}
	if strings.ContainsAny(name, `"\ `) {
		t.Fatalf("database name %q is not a bare identifier", name)
	}

	ctx := context.Background()
	admin, err := Open(ctx, base)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	drop := func() {
		_, _ = admin.Pool.Exec(ctx, `DROP DATABASE IF EXISTS "`+name+`" WITH (FORCE)`)
	}
	drop()
	if _, err := admin.Pool.Exec(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		admin.Close()
		t.Fatalf("create database %s: %v", name, err)
	}
	t.Cleanup(func() {
		drop()
		admin.Close()
	})

	u.Path = "/" + name
	return u.String()
}

func TestMigrateConcurrentBootsSerialise(t *testing.T) {
	dsn := freshDatabase(t, "sigmahub_migrate_race")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Four replicas of the same release starting milliseconds apart.
	const replicas = 4
	stores := make([]*Store, replicas)
	for i := range stores {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		st, err := Open(ctx, dsn)
		cancel()
		if err != nil {
			t.Fatalf("open replica %d: %v", i, err)
		}
		defer st.Close()
		stores[i] = st
	}

	errs := make([]error, replicas)
	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup
	for i, st := range stores {
		done.Add(1)
		go func(i int, st *Store) {
			defer done.Done()
			start.Wait()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			errs[i] = st.Migrate(ctx, log)
		}(i, st)
	}
	start.Done()
	done.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("replica %d: Migrate returned %v — a second replica booting against an unmigrated database must WAIT for the first, not fail and exit 1", i, err)
		}
	}
}
