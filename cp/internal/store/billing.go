package store

// P2-4 usage-based billing. The meter is connected-server time (server_hours,
// swept idempotently like usage_hours); the price model is one flat unit per
// connected server per month with a free-tier floor. Subscription state
// (Paddle ids + status) lives here keyed by the bare org string.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// Pricing constants — kept in sync with the web read model (a test asserts
// the two agree, like the hosting matrix).
//
// The billed quantity is UNITS, not servers: an ordinary server is one unit, a
// k8s node two and a GPU server four (server_units.go). An all-general fleet
// therefore prices exactly as it did before units existed, including the free
// tier — "your first three servers are free" stays literally true.
const (
	BillingUnitPrice  = 5 // per unit / month
	BillingFreeTier   = 3 // first N units free
	BillingCurrency   = "EUR"
	BillingHoursMonth = 730 // ~hours in a month, for server-months from server-hours
)

// SweepServerHours writes the idempotent hourly (server, hour) rows for every
// currently-connected server — the time-integrated billing meter. Idempotent
// on (server, hour), so running it several times an hour just re-touches the
// same rows (mirrors SweepUsageHours).
//
// "Connected" is status = 'running', which is what decides the two statuses
// added since: an `incompatible` host is not billed because it cannot do the
// job it was enrolled for (SIGMA-203), and a `decommissioning` one is not
// billed because the operator has told us to stop using the machine
// (SIGMA-204). The decommissioning case is a deliberate choice and not a
// side effect of the WHERE clause: the alternative — bill until the tombstone —
// charges for the minutes between pressing Disconnect and an agent finishing a
// teardown, which is time the customer neither asked for nor controls, and on
// the timeout path is time we spent waiting for a machine that never answered.
// The window is bounded by the decommission timeout either way, so the amount
// at stake is one server-hour; the reason to pick this side is that it is the
// only one we could explain on an invoice.
func (s *Store) SweepServerHours(ctx context.Context, now time.Time) (int, error) {
	tag, err := s.Pool.Exec(ctx, `
		INSERT INTO server_hours (org_id, server_id, hour)
		SELECT org_id, id, date_trunc('hour', $1::timestamptz)
		  FROM servers
		 WHERE deleted_at IS NULL AND status = 'running'
		ON CONFLICT DO NOTHING`, now.UTC())
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// ConnectedServerUnits returns the org's live fleet as a billing breakdown:
// one line per server type (sorted by type for a stable UI), the plain server
// count, and the weighted unit total the subscription bills.
//
// Counting happens per type in SQL and the weighting in Go, so the weight table
// stays the only place a weight is written down.
func (s *Store) ConnectedServerUnits(ctx context.Context, orgID string) (lines []ServerUnitLine, servers, units int, err error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT type, COUNT(*) FROM servers
		 WHERE org_id = $1 AND deleted_at IS NULL AND status = 'running'
		 GROUP BY type ORDER BY type`, orgID)
	if err != nil {
		return nil, 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var l ServerUnitLine
		if err := rows.Scan(&l.Type, &l.Count); err != nil {
			return nil, 0, 0, err
		}
		l.Weight = ServerUnitWeight(l.Type)
		l.Units = l.Count * l.Weight
		servers += l.Count
		units += l.Units
		lines = append(lines, l)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, 0, err
	}
	return lines, servers, units, nil
}

// BillingStatus is the subscription state for an org.
type BillingStatus struct {
	OrgID          string `json:"orgId"`
	CustomerID     string `json:"customerId,omitempty"`
	SubscriptionID string `json:"subscriptionId,omitempty"`
	Status         string `json:"status"` // none|active|past_due|canceled
	Quantity       int    `json:"quantity"`
}

// BillingSummary is the org's usage + charge readout for a month.
type BillingSummary struct {
	// Configured is false when no Paddle credentials are set — the UI shows an
	// explicit not-configured state instead of pretending to charge.
	Configured bool `json:"configured"`
	// Connected is the point-in-time connected-server count right now. It stays
	// a SERVER count (what the fleet looks like); Units is what it bills as.
	Connected int `json:"connected"`
	// Units is the weighted total across connected servers.
	Units int `json:"units"`
	// BillableUnits = max(0, units - free tier) — the Paddle quantity.
	BillableUnits int `json:"billableUnits"`
	// Breakdown explains Units per server type so the dashboard can show why a
	// fleet of N servers bills as M units instead of asserting a number.
	Breakdown []ServerUnitLine `json:"breakdown"`
	FreeTier  int              `json:"freeTier"`
	UnitPrice int              `json:"unitPrice"`
	Currency  string           `json:"currency"`
	// Amount is the current monthly charge (billableUnits * unitPrice).
	Amount int `json:"amount"`
	// ServerHoursThisMonth is the accrued connected time (reconciliation: what
	// was actually used vs what the subscription quantity is charging).
	ServerHoursThisMonth int           `json:"serverHoursThisMonth"`
	Month                string        `json:"month"` // YYYY-MM (UTC)
	Subscription         BillingStatus `json:"subscription"`
}

// WebhookSeen records a webhook delivery id and reports whether it was already
// seen (idempotency; reuses the P1-7 webhook_deliveries table). Returns true
// when this delivery is a duplicate that must not be re-applied.
func (s *Store) WebhookSeen(ctx context.Context, deliveryID, provider, eventType string) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `
		INSERT INTO webhook_deliveries (delivery_id, provider, event_type)
		VALUES ($1, $2, $3) ON CONFLICT (delivery_id) DO NOTHING`,
		deliveryID, provider, eventType)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 0, nil
}

// GetBillingStatus returns the org's subscription row, or a zero-value
// (status "none") when it has never subscribed.
func (s *Store) GetBillingStatus(ctx context.Context, orgID string) (BillingStatus, error) {
	st := BillingStatus{OrgID: orgID, Status: "none"}
	err := s.Pool.QueryRow(ctx, `
		SELECT paddle_customer_id, paddle_subscription_id, status, quantity
		  FROM org_billing WHERE org_id = $1`, orgID).
		Scan(&st.CustomerID, &st.SubscriptionID, &st.Status, &st.Quantity)
	if errors.Is(err, pgx.ErrNoRows) {
		return st, nil
	}
	if err != nil {
		return BillingStatus{}, err
	}
	return st, nil
}

// BillingSummaryForOrg computes the current usage + charge readout. configured
// reflects whether Paddle is wired (passed in by the handler from config).
func (s *Store) BillingSummaryForOrg(ctx context.Context, orgID string, now time.Time, configured bool) (BillingSummary, error) {
	breakdown, connected, units, err := s.ConnectedServerUnits(ctx, orgID)
	if err != nil {
		return BillingSummary{}, err
	}
	billable := units - BillingFreeTier
	if billable < 0 {
		billable = 0
	}
	monthStart := time.Date(now.UTC().Year(), now.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	var hours int
	if err := s.Pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM server_hours WHERE org_id = $1 AND hour >= $2`,
		orgID, monthStart).Scan(&hours); err != nil {
		return BillingSummary{}, err
	}
	sub, err := s.GetBillingStatus(ctx, orgID)
	if err != nil {
		return BillingSummary{}, err
	}
	return BillingSummary{
		Configured:           configured,
		Connected:            connected,
		Units:                units,
		BillableUnits:        billable,
		Breakdown:            breakdown,
		FreeTier:             BillingFreeTier,
		UnitPrice:            BillingUnitPrice,
		Currency:             BillingCurrency,
		Amount:               billable * BillingUnitPrice,
		ServerHoursThisMonth: hours,
		Month:                monthStart.Format("2006-01"),
		Subscription:         sub,
	}, nil
}

// UpsertSubscription records subscription state from a Paddle webhook (or the
// checkout flow). Audited. Enqueues a payment_failed alert on a past_due
// transition so the org is notified before anything is at risk of pausing.
func (s *Store) UpsertSubscription(ctx context.Context, orgID string, in BillingStatus, actor string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Direct (non-webhook) callers are always the newest word, so stamp now.
	if err := s.upsertSubscriptionTx(ctx, tx, orgID, in, actor, time.Now()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ApplyPaddleWebhook dedups AND applies a Paddle subscription webhook in ONE
// transaction: the delivery is recorded seen and the subscription state updated
// atomically. If the apply fails the whole tx (including the dedup marker) rolls
// back, so Paddle's retry re-applies the event instead of it being permanently
// dropped as a "duplicate" (SIGMA-90). Returns applied=false for a delivery that
// was already recorded (a genuine redelivery).
func (s *Store) ApplyPaddleWebhook(ctx context.Context, deliveryID, provider, eventType, orgID string, in BillingStatus, actor string, occurredAt time.Time) (bool, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		INSERT INTO webhook_deliveries (delivery_id, provider, event_type)
		VALUES ($1, $2, $3) ON CONFLICT (delivery_id) DO NOTHING`,
		deliveryID, provider, eventType)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		// Already recorded — commit the (no-op) tx and report duplicate.
		return false, tx.Commit(ctx)
	}
	if err := s.upsertSubscriptionTx(ctx, tx, orgID, in, actor, occurredAt); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// upsertSubscriptionTx applies subscription state within the caller's tx (shared
// by UpsertSubscription and ApplyPaddleWebhook).
func (s *Store) upsertSubscriptionTx(ctx context.Context, tx pgx.Tx, orgID string, in BillingStatus, actor string, occurredAt time.Time) error {
	var prevStatus string
	var prevEventAt *time.Time
	// FOR UPDATE (SIGMA-102): the out-of-order guard below is a read-modify-write,
	// and the Paddle handler runs one goroutine per delivery with no per-org
	// serialization. Without the row lock, two concurrent deliveries both read the
	// same pre-image and both pass the guard, and the later-committing (possibly
	// older) one wins the unconditional UPSERT — defeating SIGMA-99. The lock makes
	// the second delivery block until the first commits, then re-read the newer
	// last_event_at so the guard drops the stale event. (No row yet → nothing to
	// lock, but then prevEventAt is nil and there is no ordering to enforce; the
	// INSERT ... ON CONFLICT itself serializes concurrent creators.)
	err := tx.QueryRow(ctx, `SELECT status, last_event_at FROM org_billing WHERE org_id = $1 FOR UPDATE`, orgID).Scan(&prevStatus, &prevEventAt)
	if errors.Is(err, pgx.ErrNoRows) {
		prevStatus = "none"
	} else if err != nil {
		return err
	}
	// SIGMA-99: ignore an out-of-order (older) delivery — a delayed/retried event
	// has a distinct delivery id (so it isn't deduped), and applying it would
	// overwrite newer subscription state and re-fire a stale alert.
	if prevEventAt != nil && occurredAt.Before(*prevEventAt) {
		return nil
	}

	// The DO UPDATE is guarded by last_event_at too (SIGMA-125): the FOR UPDATE
	// read above closes the race only when a row already EXISTS to lock. Two
	// concurrent FIRST-EVER deliveries both read no row (prevEventAt nil), both
	// pass the guard above, and race to INSERT; the loser's ON CONFLICT DO UPDATE
	// would otherwise clobber the winner even if it is older. The WHERE makes the
	// stale one a no-op, and RowsAffected==0 then skips its audit/alert.
	tag, err := tx.Exec(ctx, `
		INSERT INTO org_billing (org_id, paddle_customer_id, paddle_subscription_id, status, quantity, last_event_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, now())
		ON CONFLICT (org_id) DO UPDATE SET
			paddle_customer_id     = EXCLUDED.paddle_customer_id,
			-- Never blank the stored subscription id: a transaction.* event with no
			-- subscription_id would otherwise clear it (SIGMA-103 defense-in-depth).
			paddle_subscription_id = COALESCE(NULLIF(EXCLUDED.paddle_subscription_id, ''), org_billing.paddle_subscription_id),
			status                 = EXCLUDED.status,
			quantity               = EXCLUDED.quantity,
			last_event_at          = EXCLUDED.last_event_at,
			updated_at             = now()
		WHERE org_billing.last_event_at IS NULL OR EXCLUDED.last_event_at >= org_billing.last_event_at`,
		orgID, in.CustomerID, in.SubscriptionID, in.Status, in.Quantity, occurredAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// The conflicting row was newer (a concurrent creator won with a later
		// event) — this delivery is stale, so do not audit or alert on it.
		return nil
	}
	if err := auditTx(ctx, tx, orgID, actor, "Subscription "+in.Status, in.SubscriptionID); err != nil {
		return err
	}
	if in.Status == "past_due" && prevStatus != "past_due" {
		if err := enqueueAlertTx(ctx, tx, orgID, AlertPaymentFailed,
			"billing:"+in.SubscriptionID+":past_due", 0,
			"Payment failed — subscription past due",
			"A subscription payment failed. Update the payment method in Billing; your servers keep running during the grace period, nothing is paused."); err != nil {
			return err
		}
	}
	return nil
}

// quantitySyncDebounce is how long a pushed quantity is trusted before the sweep
// will push the same value again. Paddle confirms a quantity change with a
// subscription.updated webhook, which normally lands in seconds; the debounce
// only matters when that echo is slow or lost, and bounds how often we re-PATCH
// an unchanged subscription.
const quantitySyncDebounce = 30 * time.Minute

// quantitySyncWindow is how far back a server counts as connected for billing.
// The billable count is deliberately NOT the bare point-in-time running count:
// servers flip to unreachable on a missed heartbeat, so a network blip across a
// fleet would otherwise scale the subscription down (with an immediate proration
// credit) and back up minutes later. Taking the high-water mark over the last
// day matches what the dashboard already promises — "based on the servers
// connected during the period" — and makes a scale-down take effect a day
// later, never a scale-up.
const quantitySyncWindow = 24 * time.Hour

// SubscriptionDrift is one org whose Paddle subscription is billing a quantity
// that no longer matches its connected-server count.
type SubscriptionDrift struct {
	OrgID          string
	SubscriptionID string
	// Billed is what Paddle currently charges for (its own echoed quantity).
	Billed int
	// Want is the quantity the subscription should carry.
	Want int
}

// SubscriptionsNeedingQuantitySync returns the active subscriptions whose billed
// quantity has drifted from the org's billable-server count (SIGMA-171).
//
// Only 'active' subscriptions with a stored subscription id are considered: a
// canceled or past_due subscription must not be silently re-priced, and an org
// that never checked out has nothing to update.
//
// Want is floored at 1. Paddle rejects a zero-quantity item, and auto-cancelling
// a subscription from a sweep because an org's servers went quiet would be a far
// worse failure than one month's minimum line — dropping to the free tier stays
// a deliberate act through the customer portal.
func (s *Store) SubscriptionsNeedingQuantitySync(ctx context.Context, now time.Time) ([]SubscriptionDrift, error) {
	// Both branches of the high-water mark are weighted by server type, so a
	// fleet that swaps a general server for a GPU one drifts and re-syncs even
	// though its server COUNT never moved.
	query := fmt.Sprintf(`
		WITH counts AS (
			SELECT b.org_id,
			       b.paddle_subscription_id,
			       b.quantity,
			       b.synced_quantity,
			       b.quantity_synced_at,
			       GREATEST(
			         (SELECT COALESCE(SUM(%[1]s), 0) FROM servers sv
			           WHERE sv.org_id = b.org_id AND sv.deleted_at IS NULL AND sv.status = 'running'),
			         (SELECT COALESCE(SUM(%[2]s), 0)
			            FROM (SELECT DISTINCT sh.server_id FROM server_hours sh
			                   WHERE sh.org_id = b.org_id AND sh.hour >= $1) d
			            JOIN servers sv2 ON sv2.id = d.server_id)
			       ) AS units
			  FROM org_billing b
			 WHERE b.status = 'active' AND b.paddle_subscription_id <> ''
		)
		SELECT org_id, paddle_subscription_id, quantity,
		       GREATEST(1, units - $2) AS want
		  FROM counts
		 WHERE GREATEST(1, units - $2) <> quantity
		   AND (quantity_synced_at IS NULL
		        OR quantity_synced_at < $3
		        OR synced_quantity IS DISTINCT FROM GREATEST(1, units - $2))`,
		unitWeightSQL("sv.type"), unitWeightSQL("sv2.type"))

	rows, err := s.Pool.Query(ctx, query,
		now.UTC().Add(-quantitySyncWindow), BillingFreeTier, now.UTC().Add(-quantitySyncDebounce))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SubscriptionDrift
	for rows.Next() {
		var d SubscriptionDrift
		if err := rows.Scan(&d.OrgID, &d.SubscriptionID, &d.Billed, &d.Want); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// RecordQuantitySynced notes that `quantity` was successfully pushed to Paddle.
// It deliberately does NOT touch org_billing.quantity: that column is Paddle's
// own state, ordered by last_event_at, and writing it here would make the
// confirming subscription.updated webhook look like a stale replay.
func (s *Store) RecordQuantitySynced(ctx context.Context, orgID string, quantity int, actor string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		UPDATE org_billing SET synced_quantity = $2, quantity_synced_at = now(), updated_at = now()
		 WHERE org_id = $1`, orgID, quantity); err != nil {
		return err
	}
	if err := auditTx(ctx, tx, orgID, actor, "Subscription quantity synced",
		strconv.Itoa(quantity)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
