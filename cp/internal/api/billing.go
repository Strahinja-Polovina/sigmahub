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
	summary, err := s.billing.BillingSummaryForOrg(r.Context(), orgID, time.Now(), true)
	if err != nil {
		s.writeStoreErr(w, err, "billing checkout")
		return
	}
	qty := summary.BillableServers
	if qty < 1 {
		// Nothing billable yet (within the free tier) — checkout would create a
		// zero-quantity transaction. Tell the UI honestly.
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "no billable servers yet — the first 3 connected servers are free",
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
	EventID   string          `json:"event_id"`
	EventType string          `json:"event_type"`
	Data      paddleEventData `json:"data"`
}

type paddleEventData struct {
	ID         string          `json:"id"`
	CustomerID string          `json:"customer_id"`
	Status     string          `json:"status"`
	CustomData json.RawMessage `json:"custom_data"`
	Items      []struct {
		Quantity int `json:"quantity"`
	} `json:"items"`
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

	// Idempotency: a redelivered event id is acknowledged without re-applying.
	seen, err := s.billing.WebhookSeen(r.Context(), ev.EventID, "paddle", ev.EventType)
	if err != nil {
		s.writeStoreErr(w, err, "paddle webhook dedup")
		return
	}
	if seen {
		writeJSON(w, http.StatusOK, map[string]string{"status": "duplicate"})
		return
	}

	orgID := paddleOrgID(ev.Data.CustomData)
	if orgID == "" {
		// Not correlated to an org (e.g. a customer-level event) — ack and drop.
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}
	status := paddleSubStatus(ev.EventType, ev.Data.Status)
	if status == "" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}
	qty := 0
	if len(ev.Data.Items) > 0 {
		qty = ev.Data.Items[0].Quantity
	}
	if err := s.billing.UpsertSubscription(r.Context(), orgID, store.BillingStatus{
		OrgID:          orgID,
		CustomerID:     ev.Data.CustomerID,
		SubscriptionID: ev.Data.ID,
		Status:         status,
		Quantity:       qty,
	}, "paddle-webhook"); err != nil {
		s.writeStoreErr(w, err, "paddle webhook apply")
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
func paddleSubStatus(eventType, dataStatus string) string {
	switch eventType {
	case "subscription.created", "subscription.activated", "subscription.updated", "subscription.resumed":
		switch dataStatus {
		case "active", "trialing":
			return "active"
		case "past_due":
			return "past_due"
		case "paused", "canceled":
			return "canceled"
		default:
			return "active"
		}
	case "subscription.canceled", "subscription.paused":
		return "canceled"
	case "transaction.payment_failed":
		return "past_due"
	default:
		return ""
	}
}
