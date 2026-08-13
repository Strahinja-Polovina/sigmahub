// Package billingsync keeps a Paddle subscription's billed quantity in step
// with the org's connected-server count.
//
// Before SIGMA-171 the quantity was sent outbound exactly once, from the
// checkout handler, and never again: the webhook echoed Paddle's own quantity
// back into org_billing, paddle.Client.UpdateSubscriptionQuantity had zero
// callers, and the sweep goroutine only filled meters nothing charged from. An
// org that subscribed at 4 connected servers and grew to 24 stayed billed for a
// single unit forever, while the dashboard displayed the live figure as
// "Total due" — a number Paddle was never going to invoice. Scale-down was the
// mirror image, with the customer overpaying.
package billingsync

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// Store is the store slice the reconciler needs.
type Store interface {
	SubscriptionsNeedingQuantitySync(ctx context.Context, now time.Time) ([]store.SubscriptionDrift, error)
	RecordQuantitySynced(ctx context.Context, orgID string, quantity int, actor string) error
}

// PaddleClient is the outbound Paddle surface the reconciler needs. Kept to the
// single method so a control plane with no Paddle credentials passes nil and the
// sweep is a no-op rather than a half-configured billing path.
type PaddleClient interface {
	UpdateSubscriptionQuantity(ctx context.Context, subscriptionID, priceID string, quantity int) error
}

// Syncer pushes drifted quantities to Paddle. A nil Paddle client or an empty
// price id disables it.
type Syncer struct {
	Store   Store
	Paddle  PaddleClient
	PriceID string
	Log     *slog.Logger
	// Actor is stamped on the audit entry for each synced org.
	Actor string
}

// Sync reconciles every drifted subscription once. Returns the number of
// subscriptions successfully updated.
//
// A failure on one org is logged and skipped, never fatal: the next sweep
// retries it, and one org's Paddle error must not stop the rest of the fleet
// from being billed correctly. BUT a pass that had drift to push and pushed NONE
// of it returns a non-nil error (SIGMA-365): that is the signature of a systemic
// failure — a rotated Paddle key, a wrong CP_PADDLE_PRICE_ID, a subscription
// state Paddle rejects — under which Paddle is being told the wrong quantity for
// every affected org. Surfacing it lets the usage-sweep heartbeat's last-success
// clock go stale (SIGMA-248) instead of reporting green over silent revenue
// drift. A single transient org failure amid successes stays non-fatal, so the
// heartbeat does not flap.
func (s *Syncer) Sync(ctx context.Context, now time.Time) (int, error) {
	if s == nil || s.Paddle == nil || s.PriceID == "" || s.Store == nil {
		return 0, nil
	}
	drifted, err := s.Store.SubscriptionsNeedingQuantitySync(ctx, now)
	if err != nil {
		return 0, err
	}
	actor := s.Actor
	if actor == "" {
		actor = "system:billing-sync"
	}
	synced, failed := 0, 0
	for _, d := range drifted {
		if err := s.Paddle.UpdateSubscriptionQuantity(ctx, d.SubscriptionID, s.PriceID, d.Want); err != nil {
			s.log().Error("billing sync: update quantity", "err", err, "org", d.OrgID,
				"subscription", d.SubscriptionID, "billed", d.Billed, "want", d.Want)
			failed++
			continue
		}
		// Record only after Paddle accepted it. Recording first would debounce a
		// push that never happened, leaving the org mis-billed for the whole
		// debounce window. A record failure here is NOT revenue drift — Paddle was
		// already told the right quantity — it only means the next sweep re-pushes
		// idempotently, so it does not count toward the systemic-failure signal.
		if err := s.Store.RecordQuantitySynced(ctx, d.OrgID, d.Want, actor); err != nil {
			s.log().Error("billing sync: record synced quantity", "err", err, "org", d.OrgID)
			continue
		}
		s.log().Info("billing sync: subscription quantity updated",
			"org", d.OrgID, "from", d.Billed, "to", d.Want)
		synced++
	}
	if synced == 0 && failed > 0 {
		return 0, fmt.Errorf("billing sync: all %d drifted subscription(s) failed to update", failed)
	}
	return synced, nil
}

func (s *Syncer) log() *slog.Logger {
	if s.Log == nil {
		return slog.Default()
	}
	return s.Log
}
