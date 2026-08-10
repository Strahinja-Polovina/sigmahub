package store

// Cross-replica DSD change fan-out (SIGMA-291).
//
// The reconciler's long-poll waiter map is per-process: a reconcile closes the
// channels registered in the process that rendered the change, and knows
// nothing about waiters parked in another one. That is invisible while the
// control plane runs as a single instance and silently wrong the moment it does
// not — an agent long-polling replica A is not woken by a reconcile on replica
// B, so it waits out the whole long-poll window (25s) before re-asking and
// discovering the change on its own. No error, no log line, nothing to grep
// for; deploys and config changes just feel intermittently sluggish for about
// half of all changes.
//
// Postgres LISTEN/NOTIFY is the smallest thing that fixes it: the database is
// already the coordination point for everything else the replicas share
// (advisory locks, SKIP LOCKED leases, partial unique indexes), so this adds a
// mechanism rather than a dependency. The payload is the server id, which is
// all a waiter needs to decide whether it was the one woken.
//
// Delivery is best-effort by design. NOTIFY is not durable: a listener that is
// disconnected when the notification is sent never sees it. That is acceptable
// precisely because the wake-up is an OPTIMISATION over a level-triggered
// system — a missed wake costs one long-poll window, exactly the behaviour
// every cross-replica change had before this existed, and the 60s fleet resync
// re-renders regardless.

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

// dsdChangeChannel is the NOTIFY channel every replica publishes DSD changes
// on. A constant identifier — never interpolated from input.
const dsdChangeChannel = "sigmahub_dsd"

// listenerRetryDelay backs off a dropped listener connection. Reconnecting in a
// tight loop against a database that is down would turn one outage into two.
const listenerRetryDelay = 2 * time.Second

// PublishDSDChange announces that a server's DSD changed, to every replica
// listening — including the publisher's own process.
func (s *Store) PublishDSDChange(ctx context.Context, serverID string) error {
	_, err := s.Pool.Exec(ctx, `SELECT pg_notify($1, $2)`, dsdChangeChannel, serverID)
	return err
}

// SubscribeDSDChanges blocks, calling wake with the server id of every change
// published by any replica, until ctx is cancelled. Run it in a goroutine at
// boot.
//
// It reconnects on its own: a listener that quietly died would degrade the
// fleet to timeout-paced delivery with nothing to show for it, which is the
// failure this whole file exists to remove.
func (s *Store) SubscribeDSDChanges(ctx context.Context, log *slog.Logger, wake func(serverID string)) {
	for ctx.Err() == nil {
		if err := s.listenDSDChanges(ctx, wake); err != nil && ctx.Err() == nil {
			log.Warn("dsd change listener dropped; cross-replica wake-ups pause until it reconnects", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(listenerRetryDelay):
			}
		}
	}
}

// listenDSDChanges holds one listener connection until it fails or ctx ends.
//
// The connection is opened DIRECTLY rather than taken from the pool: a LISTEN
// registration is session state, so a pooled connection handed back to the pool
// would either keep listening for whoever got it next or lose the subscription
// when it was recycled. One dedicated connection per process, outside the pool
// it must not consume.
func (s *Store) listenDSDChanges(ctx context.Context, wake func(serverID string)) error {
	conn, err := pgx.ConnectConfig(ctx, s.Pool.Config().ConnConfig.Copy())
	if err != nil {
		return err
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.Close(cctx)
	}()
	if _, err := conn.Exec(ctx, "LISTEN "+dsdChangeChannel); err != nil {
		return err
	}
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		wake(n.Payload)
	}
}
