package store

// P2-4 usage-based billing. The meter is connected-server time (server_hours,
// swept idempotently like usage_hours); the price model is one flat unit per
// connected server per month with a free-tier floor. Subscription state
// (Paddle ids + status) lives here keyed by the bare org string.

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Pricing constants — kept in sync with the web read model (a test asserts
// the two agree, like the hosting matrix).
const (
	BillingUnitPrice  = 5 // per connected server / month
	BillingFreeTier   = 3 // first N servers free
	BillingCurrency   = "EUR"
	BillingHoursMonth = 730 // ~hours in a month, for server-months from server-hours
)

// SweepServerHours writes the idempotent hourly (server, hour) rows for every
// currently-connected server — the time-integrated billing meter. Idempotent
// on (server, hour), so running it several times an hour just re-touches the
// same rows (mirrors SweepUsageHours).
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
	// Connected is the point-in-time connected-server count right now.
	Connected int `json:"connected"`
	// BillableServers = max(0, connected - free tier).
	BillableServers int    `json:"billableServers"`
	FreeTier        int    `json:"freeTier"`
	UnitPrice       int    `json:"unitPrice"`
	Currency        string `json:"currency"`
	// Amount is the current monthly charge (billableServers * unitPrice).
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
	connected, err := s.ConnectedServerCount(ctx, orgID)
	if err != nil {
		return BillingSummary{}, err
	}
	billable := connected - BillingFreeTier
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
		BillableServers:      billable,
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

	var prevStatus string
	err = tx.QueryRow(ctx, `SELECT status FROM org_billing WHERE org_id = $1`, orgID).Scan(&prevStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		prevStatus = "none"
	} else if err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO org_billing (org_id, paddle_customer_id, paddle_subscription_id, status, quantity, updated_at)
		VALUES ($1, $2, $3, $4, $5, now())
		ON CONFLICT (org_id) DO UPDATE SET
			paddle_customer_id     = EXCLUDED.paddle_customer_id,
			paddle_subscription_id = EXCLUDED.paddle_subscription_id,
			status                 = EXCLUDED.status,
			quantity               = EXCLUDED.quantity,
			updated_at             = now()`,
		orgID, in.CustomerID, in.SubscriptionID, in.Status, in.Quantity); err != nil {
		return err
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
	return tx.Commit(ctx)
}
