package api

// Billing endpoints (P2-4). The summary is member-visible; checkout/portal
// mutate the subscription so they need Org Admin. The Paddle webhook is public
// but signature-verified (mirrors the GitHub receiver). When Paddle isn't
// configured every surface degrades to an honest not-configured state.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/paddle"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// BillingStore is the store slice the billing endpoints need.
type BillingStore interface {
	BillingSummaryForOrg(ctx context.Context, orgID string, now time.Time, configured bool) (store.BillingSummary, error)
	GetBillingStatus(ctx context.Context, orgID string) (store.BillingStatus, error)
	UpsertSubscription(ctx context.Context, orgID string, in store.BillingStatus, actor string) error
	WebhookSeen(ctx context.Context, deliveryID, provider, eventType string) (bool, error)
	ApplyPaddleWebhook(ctx context.Context, deliveryID, provider, eventType, orgID string, in store.BillingStatus, actor string, occurredAt time.Time) (bool, error)
	// OrgForPaddleIDs correlates an event with no custom_data through the
	// subscription/customer ids org_billing already stores (SIGMA-293).
	OrgForPaddleIDs(ctx context.Context, subscriptionID, customerID string) (string, error)
}

const paddleWebhookMaxBytes = 5 << 20

func (s *Server) billingConfigured() bool { return s.paddle != nil }

func (s *Server) handleGetBilling(w http.ResponseWriter, r *http.Request) {
	if s.billing == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "billing is not configured"})
		return
	}
	summary, err := s.billing.BillingSummaryForOrg(r.Context(), r.PathValue("orgId"), time.Now(), s.billingConfigured())
	if err != nil {
		s.writeStoreErr(w, err, "billing summary")
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

// handleBillingCheckout creates a Paddle hosted-checkout transaction for the
// org's current billable-server quantity and returns its URL.
func (s *Server) handleBillingCheckout(w http.ResponseWriter, r *http.Request) {
	if s.paddle == nil || s.billing == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "billing is not configured"})
		return
	}
	orgID := r.PathValue("orgId")
	// SIGMA-294: refuse a SECOND subscription. Checkout never asked whether the
	// org already had one, and the reachable path to asking twice was a pause:
	// the CP recorded a paused subscription as "canceled", so the dashboard
	// offered Subscribe as the only affordance. Completing it left two Paddle
	// subscriptions on one org — org_billing holds a single
	// paddle_subscription_id, so the CP tracked one of them and the quantity
	// sweep re-priced only that one, while the customer was charged twice the
	// moment the paused one resumed.
	existing, err := s.billing.GetBillingStatus(r.Context(), orgID)
	if err != nil {
		s.writeStoreErr(w, err, "billing checkout")
		return
	}
	if existing.SubscriptionID != "" && existing.Status != "canceled" && existing.Status != "none" {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error":  "this organization already has a " + existing.Status + " subscription — manage it in the customer portal instead of subscribing again",
			"status": existing.Status,
		})
		return
	}
	summary, err := s.billing.BillingSummaryForOrg(r.Context(), orgID, time.Now(), true)
	if err != nil {
		s.writeStoreErr(w, err, "billing checkout")
		return
	}
	qty := summary.BillableUnits
	if qty < 1 {
		// Nothing billable yet (within the free tier) — checkout would create a
		// zero-quantity transaction. Tell the UI honestly.
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "no billable units yet — the first 3 units are free (an ordinary server is 1 unit)",
		})
		return
	}
	tx, err := s.paddle.CreateCheckout(r.Context(), paddle.CreateTransactionInput{
		PriceID:    s.paddlePriceID,
		Quantity:   qty,
		CustomerID: summary.Subscription.CustomerID,
		OrgID:      orgID,
	})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"checkoutUrl": tx.CheckoutURL})
}

// handleBillingPortal returns a Paddle customer-portal URL for managing the
// payment method / subscription.
func (s *Server) handleBillingPortal(w http.ResponseWriter, r *http.Request) {
	if s.paddle == nil || s.billing == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "billing is not configured"})
		return
	}
	status, err := s.billing.GetBillingStatus(r.Context(), r.PathValue("orgId"))
	if err != nil {
		s.writeStoreErr(w, err, "billing portal")
		return
	}
	if status.CustomerID == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "no subscription yet"})
		return
	}
	url, err := s.paddle.CustomerPortalURL(r.Context(), status.CustomerID)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"portalUrl": url})
}

// paddleEvent is the slice of a Paddle webhook envelope we act on.
type paddleEvent struct {
	EventID    string          `json:"event_id"`
	EventType  string          `json:"event_type"`
	OccurredAt string          `json:"occurred_at"`
	Data       paddleEventData `json:"data"`
}

type paddleEventData struct {
	ID         string          `json:"id"`
	CustomerID string          `json:"customer_id"`
	Status     string          `json:"status"`
	CustomData json.RawMessage `json:"custom_data"`
	// SubscriptionID is set on transaction.* events, where `id` is the
	// transaction id (txn_…) rather than the subscription id (sub_…) — SIGMA-103.
	SubscriptionID string `json:"subscription_id"`
	Items          []struct {
		Quantity int `json:"quantity"`
	} `json:"items"`
}

// paddleSubscriptionID returns the subscription id (sub_…) for an event. For
// subscription.* events the subscription IS the data object, so `id` is correct;
// for transaction.* events `id` is the transaction id and the subscription id
// lives in `subscription_id`. Using `id` blindly (SIGMA-103) would overwrite the
// org's stored subscription id with a txn_… on every payment-failed event.
func paddleSubscriptionID(ev paddleEvent) string {
	if strings.HasPrefix(ev.EventType, "transaction.") {
		return ev.Data.SubscriptionID
	}
	return ev.Data.ID
}

// handlePaddleWebhook verifies the Paddle-Signature and applies subscription
// state changes. Public but HMAC-verified; 503 when no secret is configured.
func (s *Server) handlePaddleWebhook(w http.ResponseWriter, r *http.Request) {
	if s.billing == nil || s.paddleWebhookSecret == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "billing webhooks are not configured"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(http.MaxBytesReader(w, r.Body, paddleWebhookMaxBytes), paddleWebhookMaxBytes))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read body"})
		return
	}
	if !paddle.VerifySignature(s.paddleWebhookSecret, body, r.Header.Get("Paddle-Signature"), time.Now(), 5*time.Minute) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid signature"})
		return
	}
	var ev paddleEvent
	if err := json.Unmarshal(body, &ev); err != nil || ev.EventID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid event"})
		return
	}

	subID := paddleSubscriptionID(ev)
	orgID := paddleOrgID(ev.Data.CustomData)
	if orgID == "" {
		// SIGMA-293: custom_data.orgId is set once, on the checkout transaction.
		// A renewal transaction, a subscription support edited in the Paddle
		// dashboard and a cancellation from the customer portal all arrive
		// without it. Dropping those meant a customer's card could expire, the
		// past_due branch never run, no payment_failed alert be enqueued, and the
		// Billing page keep saying "Active" — with Paddle not retrying, because
		// it got a 200. Fall back to the ids org_billing already stores.
		fallback, ferr := s.billing.OrgForPaddleIDs(r.Context(), subID, ev.Data.CustomerID)
		if ferr != nil {
			// A lookup failure is NOT a drop: 5xx so Paddle retries rather than
			// this event disappearing on a transient database error.
			s.writeStoreErr(w, ferr, "paddle webhook correlate")
			return
		}
		orgID = fallback
		// Repair the primary path so the NEXT event on this subscription does not
		// need the fallback. Best effort and time-boxed: a webhook ack must not
		// hang on an outbound call, and failing to stamp custom_data costs
		// nothing now that the fallback exists.
		if orgID != "" && subID != "" && s.paddle != nil {
			stampCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 3*time.Second)
			if serr := s.paddle.SetSubscriptionOrg(stampCtx, subID, orgID); serr != nil {
				s.log.Warn("paddle webhook: could not stamp orgId on subscription custom_data",
					"err", serr, "subscription_id", subID, "org", orgID)
			}
			cancel()
		}
	}
	if orgID == "" {
		// Genuinely not ours (Paddle has customer-level events that belong to no
		// org). Ack — but say so at WARN: this used to be silent at every level,
		// so a dropped payment failure could only be inferred from a subscription
		// that never changed state.
		s.log.Warn("paddle webhook: event not correlated to any org — dropped",
			"event_id", ev.EventID, "event_type", ev.EventType,
			"subscription_id", subID, "customer_id", ev.Data.CustomerID)
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}
	status := paddleSubStatus(ev.EventType, ev.Data.Status)
	if status == "" {
		s.log.Warn("paddle webhook: event type carries no subscription state — dropped",
			"event_id", ev.EventID, "event_type", ev.EventType, "org", orgID)
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}
	qty := 0
	if len(ev.Data.Items) > 0 {
		qty = ev.Data.Items[0].Quantity
	}
	// Dedup AND apply atomically: if the apply fails, the dedup marker rolls back
	// so Paddle's retry re-applies instead of being dropped as a duplicate
	// (SIGMA-90).
	// SIGMA-99: order by the event's occurred_at so a delayed/retried older
	// delivery can't clobber newer state. A missing/unparseable timestamp falls
	// back to now (treated as newest — applies).
	occurredAt := time.Now()
	if ev.OccurredAt != "" {
		if t, perr := time.Parse(time.RFC3339, ev.OccurredAt); perr == nil {
			occurredAt = t
		}
	}
	applied, err := s.billing.ApplyPaddleWebhook(r.Context(), ev.EventID, "paddle", ev.EventType, orgID, store.BillingStatus{
		OrgID:          orgID,
		CustomerID:     ev.Data.CustomerID,
		SubscriptionID: subID,
		Status:         status,
		Quantity:       qty,
	}, "paddle-webhook", occurredAt)
	if err != nil {
		s.writeStoreErr(w, err, "paddle webhook apply")
		return
	}
	if !applied {
		writeJSON(w, http.StatusOK, map[string]string{"status": "duplicate"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func paddleOrgID(custom json.RawMessage) string {
	var c struct {
		OrgID string `json:"orgId"`
	}
	_ = json.Unmarshal(custom, &c)
	return c.OrgID
}

// paddleSubStatus maps a Paddle event to our subscription status vocabulary.
//
// SIGMA-294: `paused` is its own status, NOT a synonym for `canceled`. A pause
// is reversible — this very function handles subscription.resumed — and calling
// it canceled made the dashboard offer a Subscribe button to an org that already
// had a subscription, which is how one org ended up with two.
func paddleSubStatus(eventType, dataStatus string) string {
	switch eventType {
	case "subscription.created", "subscription.activated", "subscription.updated", "subscription.resumed":
		switch dataStatus {
		case "active", "trialing":
			return "active"
		case "past_due":
			return "past_due"
		case "paused":
			return "paused"
		case "canceled":
			return "canceled"
		default:
			return "active"
		}
	case "subscription.paused":
		return "paused"
	case "subscription.canceled":
		return "canceled"
	case "transaction.payment_failed":
		return "past_due"
	default:
		return ""
	}
}
