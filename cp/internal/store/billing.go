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
	// The weight is stamped HERE, from the type the server has at the moment the
	// hour is metered, and never read off the live row again (SIGMA-346). An hour
	// is a fact about the past; re-deriving its price from the fleet's present
	// shape let a type change rewrite an already-metered window in both
	// directions. ON CONFLICT DO NOTHING keeps that immutable: re-running the
	// sweep inside the same hour cannot restamp a row.
	tag, err := s.Pool.Exec(ctx, `
		INSERT INTO server_hours (org_id, server_id, hour, unit_weight)
		SELECT org_id, id, date_trunc('hour', $1::timestamptz), `+unitWeightSQL("type")+`
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
	// none|active|past_due|paused|canceled. `paused` is deliberately distinct
	// from `canceled` (SIGMA-294): a pause is reversible, and collapsing the two
	// made the dashboard offer a Subscribe button to an org that already had a
	// subscription.
	Status   string `json:"status"`
	Quantity int    `json:"quantity"`
}

// BillingSummary is the org's usage + charge readout for a month.
type BillingSummary struct {
	// Configured is false when no Paddle credentials are set — the UI shows an
	// explicit not-configured state instead of pretending to charge.
	Configured bool `json:"configured"`
	// Connected is the point-in-time connected-server count right now. It stays
	// a SERVER count (what the fleet looks like); Units is what it bills as.
	Connected int `json:"connected"`
	// Units is the weighted total across the servers connected RIGHT NOW — what
	// Breakdown explains. It is not necessarily what is billed: see BilledUnits.
	Units int `json:"units"`
	// BilledUnits is the unit total the subscription is priced on — the
	// high-water mark of Units over the last BillingWindowHours (SIGMA-292).
	// It is >= Units whenever the fleet shrank inside the window, and it is the
	// ONLY number the invoice is derived from, so the page must show it.
	BilledUnits int `json:"billedUnits"`
	// BillingWindowHours is the width of that high-water window, published so
	// the dashboard can explain the number rather than assert it.
	BillingWindowHours int `json:"billingWindowHours"`
	// BillableUnits is the Paddle quantity — BillableQuantity(BilledUnits, …).
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

// OrgForPaddleIDs finds the org a Paddle event belongs to from the ids the event
// itself carries. Returns "" (no error) when neither id is known here.
//
// SIGMA-293: the webhook receiver used to correlate ONLY through
// data.custom_data.orgId, which is set once — on the checkout transaction. A
// renewal transaction, a subscription edited by support in the Paddle dashboard,
// and a cancellation from the customer portal all arrive without it, and every
// one of them was acked 200 and dropped. org_billing has stored
// paddle_subscription_id and paddle_customer_id since the first checkout, so the
// org was always identifiable; nothing consulted them.
//
// The subscription id wins over the customer id when both match: a customer can
// in principle back more than one org_billing row, and the subscription is the
// narrower key.
func (s *Store) OrgForPaddleIDs(ctx context.Context, subscriptionID, customerID string) (string, error) {
	if subscriptionID == "" && customerID == "" {
		return "", nil
	}
	var orgID string
	err := s.Pool.QueryRow(ctx, `
		SELECT org_id FROM org_billing
		 WHERE ($1 <> '' AND paddle_subscription_id = $1)
		    OR ($2 <> '' AND paddle_customer_id = $2)
		 ORDER BY (paddle_subscription_id = $1) DESC, updated_at DESC
		 LIMIT 1`, subscriptionID, customerID).Scan(&orgID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return orgID, nil
}

// BillingSummaryForOrg computes the current usage + charge readout. configured
// reflects whether Paddle is wired (passed in by the handler from config).
//
// BillableUnits here IS the checkout quantity and IS what the drift sweep will
// push to Paddle: all three go through BilledUnitsForOrg + BillableQuantity
// (SIGMA-292). Before that they were three formulas over two windows with two
// floors, and the number on the invoice appeared in no UI.
func (s *Store) BillingSummaryForOrg(ctx context.Context, orgID string, now time.Time, configured bool) (BillingSummary, error) {
	breakdown, connected, units, err := s.ConnectedServerUnits(ctx, orgID)
	if err != nil {
		return BillingSummary{}, err
	}
	billedUnits, err := s.BilledUnitsForOrg(ctx, orgID, now)
	if err != nil {
		return BillingSummary{}, err
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
	billable := BillableQuantity(billedUnits, SubscriptionHoldsMinimum(sub))
	return BillingSummary{
		Configured:           configured,
		Connected:            connected,
		Units:                units,
		BilledUnits:          billedUnits,
		BillingWindowHours:   int(quantitySyncWindow / time.Hour),
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
		INSERT INTO org_billing (org_id, paddle_customer_id, paddle_subscription_id, status, quantity, last_event_at, status_since, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, now(), now())
		ON CONFLICT (org_id) DO UPDATE SET
			paddle_customer_id     = EXCLUDED.paddle_customer_id,
			-- Never blank the stored subscription id: a transaction.* event with no
			-- subscription_id would otherwise clear it (SIGMA-103 defense-in-depth).
			paddle_subscription_id = COALESCE(NULLIF(EXCLUDED.paddle_subscription_id, ''), org_billing.paddle_subscription_id),
			status                 = EXCLUDED.status,
			quantity               = EXCLUDED.quantity,
			last_event_at          = EXCLUDED.last_event_at,
			-- The grace clock (SIGMA-295) restarts only on a real status CHANGE.
			-- Re-applying the same status — Paddle re-sends subscription.updated
			-- for quantity edits — must not hand a delinquent org another two
			-- weeks. dunning_last_at resets with it so a NEW delinquency reminds
			-- on its own schedule instead of inheriting the previous one's.
			status_since           = CASE WHEN org_billing.status IS DISTINCT FROM EXCLUDED.status
			                              THEN now() ELSE org_billing.status_since END,
			dunning_last_at        = CASE WHEN org_billing.status IS DISTINCT FROM EXCLUDED.status
			                              THEN NULL ELSE org_billing.dunning_last_at END,
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
	// SIGMA-295: a delinquency is announced on ENTRY here, and then repeated by
	// SweepBillingDunning until it is resolved or the grace period expires. A
	// cancellation used to announce nothing at all — the org simply stopped
	// paying and kept everything, and neither side was told.
	if in.Status != prevStatus {
		if title, body, ok := dunningNotice(in.Status, BillingGracePeriod); ok {
			if err := enqueueAlertTx(ctx, tx, orgID, AlertPaymentFailed,
				dunningDedupKey(in.SubscriptionID, in.Status), BillingDunningInterval,
				title, body); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx,
				`UPDATE org_billing SET dunning_last_at = now() WHERE org_id = $1`, orgID); err != nil {
				return err
			}
		}
	}
	return nil
}

// ── Dunning: what actually happens when an org stops paying (SIGMA-295) ─────
//
// The policy, written down so the behaviour is deliberate rather than emergent:
//
//  1. Entering past_due or canceled alerts the org immediately (UpsertSubscription).
//  2. SweepBillingDunning repeats that alert every BillingDunningInterval for as
//     long as the org stays delinquent. One alert, ever, was the whole sequence
//     before this, and it fired into the CUSTOMER's channels — the operator was
//     never told at all.
//  3. After BillingGracePeriod the org is CAPPED: it may not add servers or
//     provision new resources. Everything already running keeps running —
//     deploys, certificates, backups and restores are what a customer would lose
//     data over, and we do not hold data hostage over an expired card.
//  4. DelinquentOrgs is the operator's list, so a non-paying tenant is something
//     you can see rather than something you reconcile against Paddle by hand.
//
// Resolving the delinquency (a successful payment, a resumed subscription) moves
// the status back to active, which resets status_since and clears the cap.
const (
	// BillingGracePeriod is how long full service continues after a subscription
	// goes past_due or canceled. Two weeks is longer than Paddle's own retry
	// schedule for a failed card, so a customer whose card simply expired gets
	// through the retries before anything they can notice changes.
	BillingGracePeriod = 14 * 24 * time.Hour
	// BillingDunningInterval is how often the reminder repeats while delinquent.
	BillingDunningInterval = 72 * time.Hour
)

// ErrBillingCapped is returned when an org past its grace period tries to grow.
// It is a distinct type (not ErrInvalid) so the API can answer 402 Payment
// Required and the message can say exactly what to do about it.
type ErrBillingCapped struct {
	OrgID      string
	Status     string
	Since      time.Time
	GraceUntil time.Time
}

func (e ErrBillingCapped) Error() string {
	what := "payment is past due"
	if e.Status == "canceled" {
		what = "the subscription was canceled"
	}
	return fmt.Sprintf("billing is not current for this organization (%s since %s; the grace period ended %s) — "+
		"existing servers, deploys and backups keep running, but new servers and resources are paused until billing is restored in the customer portal",
		what, e.Since.UTC().Format("2006-01-02"), e.GraceUntil.UTC().Format("2006-01-02"))
}

// billingDelinquent reports whether a subscription status means "not paying".
// `paused` is deliberately NOT here: a pause is an agreed, reversible state the
// customer arranged, not a failure to pay (SIGMA-294).
func billingDelinquent(status string) bool {
	return status == "past_due" || status == "canceled"
}

// dunningDedupKey is the alert dedup key for a delinquency. `suffix` separates
// the transition alert from the repeating reminder so the reminder's schedule is
// not swallowed by the transition's.
func dunningDedupKey(subscriptionID, status string, suffix ...string) string {
	key := "billing:" + subscriptionID + ":" + status
	for _, s := range suffix {
		key += ":" + s
	}
	return key
}

// dunningNotice is the alert text for a delinquency, or ok=false for a status
// that is not one. capped changes the message from "nothing is paused" to what
// actually is — saying "nothing is paused" after the cap engaged would be the
// same dishonesty the alert exists to avoid.
func dunningNotice(status string, remaining time.Duration) (title, body string, ok bool) {
	capped := remaining <= 0
	switch status {
	case "past_due":
		title = "Payment failed — subscription past due"
		if capped {
			body = "A subscription payment is still outstanding and the grace period has ended. " +
				"Your existing servers, deploys and backups keep running, but new servers and resources are paused. " +
				"Update the payment method in the customer portal to restore them."
			return title, body, true
		}
		body = fmt.Sprintf("A subscription payment failed. Update the payment method in Billing; your servers keep running "+
			"for the next %d days, nothing is paused. After that, new servers and resources are paused — existing ones keep running.",
			int(remaining.Hours()/24))
		return title, body, true
	case "canceled":
		title = "Subscription canceled"
		if capped {
			body = "This organization's subscription is canceled and the grace period has ended. " +
				"Existing servers, deploys and backups keep running, but new servers and resources are paused. " +
				"Subscribe again from the Billing page to restore them."
			return title, body, true
		}
		body = fmt.Sprintf("This organization's subscription was canceled. Everything keeps running for the next %d days; "+
			"after that, new servers and resources are paused (existing ones keep running). Subscribe again from the Billing page to avoid it.",
			int(remaining.Hours()/24))
		return title, body, true
	default:
		return "", "", false
	}
}

// DelinquentOrg is one non-paying tenant, for the operator's view.
type DelinquentOrg struct {
	OrgID          string    `json:"orgId"`
	Status         string    `json:"status"`
	SubscriptionID string    `json:"subscriptionId,omitempty"`
	Since          time.Time `json:"since"`
	GraceExpiresAt time.Time `json:"graceExpiresAt"`
	// Capped is true once the grace period has expired — the org can no longer
	// add servers or resources.
	Capped bool `json:"capped"`
	// Servers/Units are what the org is still consuming while not paying, and
	// MonthlyValue what that fleet would bill at the current price. This is the
	// number that makes the list actionable: it is the revenue being lost and
	// the infrastructure cost still being incurred.
	Servers      int    `json:"servers"`
	Units        int    `json:"units"`
	MonthlyValue int    `json:"monthlyValue"`
	Currency     string `json:"currency"`
}

// DelinquentOrgs lists every org whose subscription is past_due or canceled,
// oldest delinquency first. This is the operator-visible half of SIGMA-295:
// before it, the only way to find a non-paying tenant was to reconcile Paddle
// against org_billing by hand.
func (s *Store) DelinquentOrgs(ctx context.Context, now time.Time) ([]DelinquentOrg, error) {
	query := fmt.Sprintf(`
		SELECT b.org_id, b.status, b.paddle_subscription_id, b.status_since,
		       (SELECT COUNT(*) FROM servers sv
		         WHERE sv.org_id = b.org_id AND sv.deleted_at IS NULL AND sv.status = 'running'),
		       %s
		  FROM org_billing b
		 WHERE b.status IN ('past_due', 'canceled')
		 ORDER BY b.status_since`, billedUnitsSQL("b.org_id", "$1"))
	rows, err := s.Pool.Query(ctx, query, now.UTC().Add(-quantitySyncWindow))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DelinquentOrg
	for rows.Next() {
		var d DelinquentOrg
		if err := rows.Scan(&d.OrgID, &d.Status, &d.SubscriptionID, &d.Since, &d.Servers, &d.Units); err != nil {
			return nil, err
		}
		d.GraceExpiresAt = d.Since.Add(BillingGracePeriod)
		d.Capped = !now.UTC().Before(d.GraceExpiresAt)
		// Valued as if they were paying — hasSubscription=false, because the
		// point of the number is the revenue at stake, not a minimum line.
		d.MonthlyValue = BillableQuantity(d.Units, false) * BillingUnitPrice
		d.Currency = BillingCurrency
		out = append(out, d)
	}
	return out, rows.Err()
}

// SweepBillingDunning re-alerts every delinquent org whose last reminder is
// older than BillingDunningInterval, and returns how many were reminded.
//
// The transition alert in upsertSubscriptionTx fires once, on entry. Without
// this sweep that WAS the entire dunning sequence: a customer whose card failed
// got one Slack message and then silence, indefinitely, while the control plane
// kept deploying, renewing certificates and running backups for free.
func (s *Store) SweepBillingDunning(ctx context.Context, now time.Time) (int, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT org_id, status, paddle_subscription_id, status_since
		  FROM org_billing
		 WHERE status IN ('past_due', 'canceled')
		   AND (dunning_last_at IS NULL OR dunning_last_at <= $1)
		 ORDER BY status_since`, now.UTC().Add(-BillingDunningInterval))
	if err != nil {
		return 0, err
	}
	type due struct {
		orgID, status, subID string
		since                time.Time
	}
	var pending []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.orgID, &d.status, &d.subID, &d.since); err != nil {
			rows.Close()
			return 0, err
		}
		pending = append(pending, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	reminded := 0
	for _, d := range pending {
		remaining := d.since.Add(BillingGracePeriod).Sub(now.UTC())
		title, body, ok := dunningNotice(d.status, remaining)
		if !ok {
			continue
		}
		// Per-org transaction: the alert and the cursor move together, so a
		// failure re-reminds next pass rather than silently skipping a cycle.
		if err := func() error {
			tx, err := s.Pool.Begin(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if err := enqueueAlertTx(ctx, tx, d.orgID, AlertPaymentFailed,
				dunningDedupKey(d.subID, d.status, "dunning"), BillingDunningInterval,
				title, body); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE org_billing SET dunning_last_at = $2 WHERE org_id = $1`,
				d.orgID, now.UTC()); err != nil {
				return err
			}
			return tx.Commit(ctx)
		}(); err != nil {
			return reminded, err
		}
		reminded++
	}
	return reminded, nil
}

// assertBillingNotCappedTx refuses growth for an org whose grace period has
// expired. Called from precreateServerTx, the single chokepoint both
// server-creation paths go through.
//
// Nothing anywhere used to consult org_billing.status before creating a server,
// provisioning a resource or running a deploy — a canceled 40-unit fleet kept
// every capability forever (SIGMA-295).
func assertBillingNotCappedTx(ctx context.Context, tx pgx.Tx, orgID string, now time.Time) error {
	var status string
	var since time.Time
	err := tx.QueryRow(ctx,
		`SELECT status, status_since FROM org_billing WHERE org_id = $1`, orgID).Scan(&status, &since)
	if errors.Is(err, pgx.ErrNoRows) {
		// Never subscribed. The free tier is a product, not a delinquency.
		return nil
	}
	if err != nil {
		return err
	}
	if !billingDelinquent(status) {
		return nil
	}
	graceUntil := since.Add(BillingGracePeriod)
	if now.UTC().Before(graceUntil) {
		// Inside the grace period the past_due alert's promise holds literally:
		// your servers keep running and nothing is paused.
		return nil
	}
	return ErrBillingCapped{OrgID: orgID, Status: status, Since: since, GraceUntil: graceUntil}
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

// SubscriptionMinQuantity is the smallest quantity an EXISTING subscription can
// carry. Paddle rejects a zero-quantity item, and auto-cancelling a subscription
// from a sweep because an org's servers went quiet would be a far worse failure
// than one month's minimum line — dropping to the free tier stays a deliberate
// act through the customer portal.
//
// It is a floor on the invoice, so it is also a floor on what the dashboard and
// the checkout show. Before SIGMA-292 only the sweep knew about it: an org that
// shrank below the free tier saw "0 billable units · nothing due" on the Billing
// page while Paddle kept invoicing one unit every month.
const SubscriptionMinQuantity = 1

// BillableQuantity is THE definition of the quantity Paddle bills for a weighted
// unit total. Every site that answers "how many units does this org owe for" —
// the Billing summary, the checkout transaction and the drift sweep — resolves
// through this function (the sweep through billableQuantitySQL, which renders
// the same arithmetic from the same constants).
//
// hasSubscription is true when the org already has a live subscription the sweep
// will hold at SubscriptionMinQuantity; without one, a fleet inside the free tier
// genuinely owes nothing and checkout must refuse rather than sell a minimum.
func BillableQuantity(units int, hasSubscription bool) int {
	q := units - BillingFreeTier
	if q < 0 {
		q = 0
	}
	if hasSubscription && q < SubscriptionMinQuantity {
		q = SubscriptionMinQuantity
	}
	return q
}

// SubscriptionHoldsMinimum reports whether this subscription is one the quantity
// sweep will keep alive at SubscriptionMinQuantity. It must match the WHERE
// clause of SubscriptionsNeedingQuantitySync exactly, or the summary and the
// sweep disagree about the floor again.
func SubscriptionHoldsMinimum(sub BillingStatus) bool {
	return sub.Status == "active" && sub.SubscriptionID != ""
}

// billedUnitsSQL renders the org's BILLED unit total — the high-water mark of
// weighted units over quantitySyncWindow — as a scalar SQL expression. orgExpr
// is the org id (a correlated column in the sweep, a bind parameter for a single
// org) and sinceExpr the start of the window.
//
// Some of this arithmetic has to happen in the database (the sweep runs over
// every org in one statement), so it cannot literally be Go code; rendering the
// expression once from the weight table keeps the summary and the sweep reading
// the same definition instead of two hand-copied ones that drift (SIGMA-292).
// The live arm still weights from servers.type, and correctly so: it prices what
// is running NOW. The historical arm reads the weight stamped on each metered
// hour (SIGMA-346) rather than joining back to the live row, so a type change
// cannot re-price hours that are already in the past. Per server it takes the
// MAX weight seen in the window, which is what a high-water mark means: a host
// that spent part of the window as a GPU peaked as a GPU.
//
// Dropping the JOIN to servers also stops a metered hour from vanishing if its
// server row ever disappears outright — soft deletes are all production does
// today, so this changes no figure, but the meter should not depend on it.
func billedUnitsSQL(orgExpr, sinceExpr string) string {
	return fmt.Sprintf(`GREATEST(
		         (SELECT COALESCE(SUM(%[1]s), 0) FROM servers sv
		           WHERE sv.org_id = %[2]s AND sv.deleted_at IS NULL AND sv.status = 'running'),
		         (SELECT COALESCE(SUM(d.w), 0)
		            FROM (SELECT MAX(sh.unit_weight) AS w FROM server_hours sh
		                   WHERE sh.org_id = %[2]s AND sh.hour >= %[3]s
		                   GROUP BY sh.server_id) d)
		       )`, unitWeightSQL("sv.type"), orgExpr, sinceExpr)
}

// billableQuantitySQL is BillableQuantity(unitsExpr, true) in SQL — the sweep
// only ever looks at orgs that already have a live subscription, so the
// SubscriptionMinQuantity floor always applies there.
func billableQuantitySQL(unitsExpr string) string {
	return fmt.Sprintf("GREATEST(%d, %s - %d)", SubscriptionMinQuantity, unitsExpr, BillingFreeTier)
}

// BilledUnitsForOrg returns the unit total the org's subscription is priced on:
// the high-water mark of weighted connected units over quantitySyncWindow. This
// is the input to BillableQuantity for the summary and the checkout, and the
// same expression the sweep evaluates per org.
func (s *Store) BilledUnitsForOrg(ctx context.Context, orgID string, now time.Time) (int, error) {
	var units int
	err := s.Pool.QueryRow(ctx, `SELECT `+billedUnitsSQL("$1::text", "$2::timestamptz"),
		orgID, now.UTC().Add(-quantitySyncWindow)).Scan(&units)
	if err != nil {
		return 0, err
	}
	return units, nil
}

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
// Want is floored at SubscriptionMinQuantity and computed by the same
// billedUnitsSQL/billableQuantitySQL pair BillingSummaryForOrg goes through, so
// the quantity this sweep PATCHes is the quantity the customer saw at checkout
// and sees on the Billing page (SIGMA-292).
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
			       %[1]s AS units
			  FROM org_billing b
			 WHERE b.status = 'active' AND b.paddle_subscription_id <> ''
		)
		SELECT org_id, paddle_subscription_id, quantity,
		       %[2]s AS want
		  FROM counts
		 WHERE %[2]s <> quantity
		   AND (quantity_synced_at IS NULL
		        OR quantity_synced_at < $2
		        OR synced_quantity IS DISTINCT FROM %[2]s)`,
		billedUnitsSQL("b.org_id", "$1"), billableQuantitySQL("units"))

	rows, err := s.Pool.Query(ctx, query,
		now.UTC().Add(-quantitySyncWindow), now.UTC().Add(-quantitySyncDebounce))
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
