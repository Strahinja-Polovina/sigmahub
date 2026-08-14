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
	"sync"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// What separates "Paddle is broken for everyone" from "one subscription is
// unpushable" (SIGMA-365).
//
// The systemic signal exists to make a rotated API key or a wrong price id go
// loud. Its first form — any pass with drift that synced nothing — fired on a
// population of one, and a population of one is the common case: most passes
// have a single drifted org, and a subscription Paddle permanently rejects (one
// canceled out from under us, a price mismatch on one plan) then failed EVERY
// pass forever. The usage-sweep heartbeat stayed red from then on, so the alert
// that was supposed to mean "nobody is being billed correctly" came to mean
// "org_x still exists", and the real event it was built for would have arrived
// into an already-red channel with nothing to distinguish it.
//
// The first fix for that required a POPULATION — three drifted subscriptions
// before a pass could be called systemic. That is wrong in the direction that
// matters at launch: a control plane with two paying customers never has three
// drifted subscriptions, so a rotated key would have stopped all billing in
// total silence on exactly the deployments where every customer counts. And the
// population never was the discriminator; with one subscription, "one bad
// tenant" and "everything is broken" are the same observation.
//
// What actually distinguishes the noise from the signal is not how many orgs
// fail but how the alert BEHAVES once it fires. A permanently-failing
// subscription must not hold the heartbeat red forever — it must alert, then let
// the per-org backoff quiet it down, and alert again on a widening interval for
// as long as it stays broken. So the signal needs a unanimous verdict and
// persistence, and the backoff below supplies the cadence.
const (
	// Consecutive qualifying passes before the error is returned. At the sweep's
	// 10-minute tick this is ~30 minutes of every push failing — long enough that
	// a Paddle blip does not flap the heartbeat, short enough to be well inside
	// any billing period.
	systemicPasses = 3
	// Cap on how many passes an individually-failing org is skipped for. ~80
	// minutes at the 10-minute tick: long enough to stop hammering a
	// subscription Paddle will never accept, short enough that an org whose
	// problem was fixed re-syncs without an operator doing anything.
	maxCooldownPasses = 8
)

// orgState is the in-memory backoff for one org. Deliberately not persisted: it
// is an optimisation, and a control plane that restarts should give every
// subscription a fresh attempt rather than inherit a verdict from a process that
// may have been failing for its own reasons.
type orgState struct {
	failures int
	cooldown int
}

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

	// Cross-pass state. The sweep is one goroutine, but the Syncer is a shared
	// pointer and a future caller should not have to discover that by racing.
	mu             sync.Mutex
	orgs           map[string]*orgState
	systemicStreak int
}

// Sync reconciles every drifted subscription once. Returns the number of
// subscriptions successfully updated.
//
// A failure on one org is logged and skipped, never fatal: the next sweep
// retries it, and one org's Paddle error must not stop the rest of the fleet
// from being billed correctly. A pass that had drift to push and pushed NONE of
// it is the signature of a SYSTEMIC failure — a rotated Paddle key, a wrong
// CP_PADDLE_PRICE_ID — under which Paddle is being told the wrong quantity for
// every affected org, and that is worth failing the pass over: it lets the
// usage-sweep heartbeat's last-success clock go stale (SIGMA-248) instead of
// reporting green over silent revenue drift.
//
// But "pushed none of it" only means that over a population. See the constants
// above for the three conditions a pass now has to meet before it is called
// systemic, and for the per-org backoff that keeps one unpushable subscription
// from being retried every ten minutes until someone notices.
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

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.orgs == nil {
		s.orgs = map[string]*orgState{}
	}

	synced, failed, deferred := 0, 0, 0
	seen := make(map[string]bool, len(drifted))
	var failedNow []string
	for _, d := range drifted {
		seen[d.OrgID] = true
		st := s.orgs[d.OrgID]
		if st != nil && st.cooldown > 0 {
			st.cooldown--
			deferred++
			continue
		}
		if err := s.Paddle.UpdateSubscriptionQuantity(ctx, d.SubscriptionID, s.PriceID, d.Want); err != nil {
			s.log().Error("billing sync: update quantity", "err", err, "org", d.OrgID,
				"subscription", d.SubscriptionID, "billed", d.Billed, "want", d.Want)
			if st == nil {
				st = &orgState{}
				s.orgs[d.OrgID] = st
			}
			st.failures++
			failed++
			failedNow = append(failedNow, d.OrgID)
			continue
		}
		// Record only after Paddle accepted it. Recording first would debounce a
		// push that never happened, leaving the org mis-billed for the whole
		// debounce window. A record failure here is NOT revenue drift — Paddle was
		// already told the right quantity — it only means the next sweep re-pushes
		// idempotently, so it does not count toward the systemic-failure signal.
		if err := s.Store.RecordQuantitySynced(ctx, d.OrgID, d.Want, actor); err != nil {
			s.log().Error("billing sync: record synced quantity", "err", err, "org", d.OrgID)
			delete(s.orgs, d.OrgID)
			continue
		}
		s.log().Info("billing sync: subscription quantity updated",
			"org", d.OrgID, "from", d.Billed, "to", d.Want)
		delete(s.orgs, d.OrgID)
		synced++
	}

	// An org that stopped drifting — it re-synced, it was cancelled, its fleet
	// moved back — must not carry a failure history into the next time it needs
	// a push.
	for orgID := range s.orgs {
		if !seen[orgID] {
			delete(s.orgs, orgID)
		}
	}

	if deferred > 0 {
		// Not silent: a subscription being skipped is a subscription being billed
		// the wrong amount, and the only reason it is acceptable is that it is
		// bounded and visible.
		s.log().Warn("billing sync: subscriptions skipped while backing off", "count", deferred)
	}
	attempted := synced + failed

	// Back off the orgs that failed IN THIS PASS. Unconditionally: this is what
	// keeps a permanently-broken subscription from being retried every ten
	// minutes forever, AND — because the streak below only moves on passes that
	// attempted something — it is what turns a stuck alert into a widening one.
	//
	// Only this pass's failures, never every org in the map: an org that was
	// DEFERRED this pass has just decremented its way to a retry, and re-arming
	// it here would hold it at one pass short of an attempt forever.
	for _, orgID := range failedNow {
		st := s.orgs[orgID]
		if st.failures > 1 {
			st.cooldown = min(1<<(st.failures-2), maxCooldownPasses)
			s.log().Warn("billing sync: backing off a subscription that keeps failing",
				"org", orgID, "consecutive_failures", st.failures,
				"skipping_passes", st.cooldown)
		}
	}

	// The streak counts EVIDENCE, not passes. A pass that attempted nothing —
	// every drifted org in backoff, or no drift at all — proves nothing either
	// way, so it must neither advance the streak nor clear it. Clearing it there
	// was the trap in the first design: backoff would have starved the streak of
	// the very attempts that demonstrate an outage, and the alert could never
	// have fired at all.
	switch {
	case attempted == 0:
		// no evidence this pass; hold whatever we had
	case synced == 0:
		s.systemicStreak++
	default:
		// Something got through. Whatever is wrong is not "nothing is billing".
		s.systemicStreak = 0
	}
	if s.systemicStreak >= systemicPasses {
		// Re-arm rather than latch. The condition is still true next pass, and an
		// alert that fires every ten minutes until someone fixes Paddle is how a
		// channel gets muted. With the backoff above, the next report is several
		// passes away, and further away again after that — which is the cadence a
		// human actually wants from "still broken".
		s.systemicStreak = 0
		return synced, fmt.Errorf(
			"billing sync: all %d attempted subscription update(s) failed on %d consecutive passes — "+
				"check CP_PADDLE_API_KEY and CP_PADDLE_PRICE_ID", failed, systemicPasses)
	}
	return synced, nil
}

func (s *Syncer) log() *slog.Logger {
	if s.Log == nil {
		return slog.Default()
	}
	return s.Log
}
