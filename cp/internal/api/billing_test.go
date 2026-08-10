package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/paddle"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

type fakeBilling struct {
	summary   store.BillingSummary
	status    store.BillingStatus
	applied   []store.BillingStatus
	seen      map[string]bool
	seenCalls int
	// orgByPaddleID stands in for the org_billing rows the webhook receiver
	// falls back to when an event carries no custom_data (SIGMA-293), keyed by
	// paddle subscription id and by paddle customer id.
	orgByPaddleID map[string]string
	// appliedOrgs records the org each apply was correlated to.
	appliedOrgs []string
}

func (f *fakeBilling) BillingSummaryForOrg(_ context.Context, orgID string, _ time.Time, configured bool) (store.BillingSummary, error) {
	s := f.summary
	s.Configured = configured
	return s, nil
}
func (f *fakeBilling) GetBillingStatus(context.Context, string) (store.BillingStatus, error) {
	if f.status.Status == "" {
		return store.BillingStatus{Status: "none"}, nil
	}
	return f.status, nil
}
func (f *fakeBilling) UpsertSubscription(_ context.Context, orgID string, in store.BillingStatus, _ string) error {
	f.applied = append(f.applied, in)
	return nil
}
func (f *fakeBilling) WebhookSeen(_ context.Context, deliveryID, _, _ string) (bool, error) {
	f.seenCalls++
	if f.seen == nil {
		f.seen = map[string]bool{}
	}
	if f.seen[deliveryID] {
		return true, nil
	}
	f.seen[deliveryID] = true
	return false, nil
}
func (f *fakeBilling) ApplyPaddleWebhook(_ context.Context, deliveryID, _, _, orgID string, in store.BillingStatus, _ string, _ time.Time) (bool, error) {
	f.seenCalls++
	if f.seen == nil {
		f.seen = map[string]bool{}
	}
	if f.seen[deliveryID] {
		return false, nil
	}
	f.seen[deliveryID] = true
	f.applied = append(f.applied, in)
	f.appliedOrgs = append(f.appliedOrgs, orgID)
	return true, nil
}
func (f *fakeBilling) OrgForPaddleIDs(_ context.Context, subscriptionID, customerID string) (string, error) {
	if org, ok := f.orgByPaddleID[subscriptionID]; ok && subscriptionID != "" {
		return org, nil
	}
	if org, ok := f.orgByPaddleID[customerID]; ok && customerID != "" {
		return org, nil
	}
	return "", nil
}

func billingServer(t *testing.T, fb *fakeBilling, secret string) *Server {
	t.Helper()
	return New(slog.Default(), fakePinger{}, &fakeStore{}, &fakeDomain{}, Options{
		DevServiceToken:     testServiceToken,
		Billing:             fb,
		PaddleWebhookSecret: secret,
	})
}

func TestBillingSummaryHonestOff(t *testing.T) {
	// No billing store at all → 503.
	s := New(slog.Default(), fakePinger{}, &fakeStore{}, &fakeDomain{}, Options{DevServiceToken: testServiceToken})
	req := httptest.NewRequest("GET", "/v1/orgs/org_1/billing", nil)
	req.Header.Set("Authorization", "Bearer "+testServiceToken)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no-billing summary = %d, want 503", rec.Code)
	}

	// Billing store present but no Paddle client → configured=false.
	fb := &fakeBilling{summary: store.BillingSummary{Connected: 4, Units: 4, BillableUnits: 1, Amount: 5}}
	s = billingServer(t, fb, "")
	req = httptest.NewRequest("GET", "/v1/orgs/org_1/billing", nil)
	req.Header.Set("Authorization", "Bearer "+testServiceToken)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("billing summary = %d", rec.Code)
	}
	var out store.BillingSummary
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Configured {
		t.Fatal("no Paddle client → configured must be false")
	}
	// Checkout without a Paddle client → 503.
	req = httptest.NewRequest("POST", "/v1/orgs/org_1/billing/checkout", nil)
	req.Header.Set("Authorization", "Bearer "+testServiceToken)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("checkout without paddle = %d, want 503", rec.Code)
	}
}

func TestPaddleWebhookVerifyAndIdempotency(t *testing.T) {
	const secret = "pdl_secret"
	// No secret configured → 503.
	off := billingServer(t, &fakeBilling{}, "")
	req := httptest.NewRequest("POST", "/v1/webhooks/paddle", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	off.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("webhook without secret = %d, want 503", rec.Code)
	}

	fb := &fakeBilling{}
	s := billingServer(t, fb, secret)
	body := []byte(`{"event_id":"evt_1","event_type":"subscription.updated","data":{"id":"sub_9","customer_id":"ctm_9","status":"active","custom_data":{"orgId":"org_1"},"items":[{"quantity":3}]}}`)
	ts := "1700000000"
	now := time.Unix(1_700_000_000, 0)

	post := func(sig string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/v1/webhooks/paddle", strings.NewReader(string(body)))
		r.Header.Set("Paddle-Signature", sig)
		w := httptest.NewRecorder()
		// Re-verify against the same clock the handler uses is impossible to
		// pin here; the signature is computed for `now` and the handler uses
		// time.Now(), so tolerate skew by signing with a fresh timestamp.
		s.Handler().ServeHTTP(w, r)
		return w
	}

	// A bad signature is rejected 401 (uses a real recent timestamp).
	realTs := time.Now().Unix()
	bad := post(paddleSig(secret, realTs, []byte("tampered")))
	if bad.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature = %d, want 401", bad.Code)
	}

	good := post(paddleSig(secret, realTs, body))
	if good.Code != http.StatusOK {
		t.Fatalf("valid webhook = %d body %s", good.Code, good.Body)
	}
	if len(fb.applied) != 1 || fb.applied[0].Status != "active" || fb.applied[0].SubscriptionID != "sub_9" || fb.applied[0].Quantity != 3 {
		t.Fatalf("applied = %+v", fb.applied)
	}

	// Redelivery of the same event id is acknowledged without re-applying.
	dup := post(paddleSig(secret, realTs, body))
	if dup.Code != http.StatusOK {
		t.Fatalf("duplicate webhook = %d", dup.Code)
	}
	if len(fb.applied) != 1 {
		t.Fatalf("duplicate must not re-apply, applied = %d", len(fb.applied))
	}
	_ = now
	_ = ts
}

// TestPaddleTransactionEventUsesSubscriptionID covers SIGMA-103: for a
// transaction.* event, data.id is a transaction id (txn_…) and the real
// subscription id is in data.subscription_id. The applied BillingStatus must
// carry the sub_… id, never the txn_… id (which would corrupt the stored
// subscription id keyed on by billing reads, alerts, and Paddle operations).
func TestPaddleTransactionEventUsesSubscriptionID(t *testing.T) {
	const secret = "pdl_secret"
	fb := &fakeBilling{}
	s := billingServer(t, fb, secret)
	body := []byte(`{"event_id":"evt_tx","event_type":"transaction.payment_failed","data":{"id":"txn_777","customer_id":"ctm_9","subscription_id":"sub_9","custom_data":{"orgId":"org_1"}}}`)
	realTs := time.Now().Unix()
	r := httptest.NewRequest("POST", "/v1/webhooks/paddle", strings.NewReader(string(body)))
	r.Header.Set("Paddle-Signature", paddleSig(secret, realTs, body))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("transaction webhook = %d body %s", w.Code, w.Body)
	}
	if len(fb.applied) != 1 {
		t.Fatalf("applied = %d, want 1", len(fb.applied))
	}
	if fb.applied[0].SubscriptionID != "sub_9" {
		t.Fatalf("subscription id = %q, want sub_9 (never the txn id)", fb.applied[0].SubscriptionID)
	}
	if fb.applied[0].Status != "past_due" {
		t.Fatalf("status = %q, want past_due", fb.applied[0].Status)
	}
}

// TestPaddleWebhook_CorrelatesBySubscriptionID covers SIGMA-293: a RENEWAL
// transaction carries no custom_data — orgId was attached to the original
// checkout transaction, not to this one — so correlating solely through
// custom_data.orgId dropped the event with a 200 and no log line. The customer's
// card expired, org_billing stayed 'active', no payment_failed alert was
// enqueued, the Billing page kept saying "Active", and Paddle did not retry
// because it got a 200. org_billing already stores the subscription and customer
// ids the event carries; either identifies the org.
func TestPaddleWebhook_CorrelatesBySubscriptionID(t *testing.T) {
	const secret = "pdl_secret"
	fb := &fakeBilling{orgByPaddleID: map[string]string{"sub_9": "org_1"}}
	s := billingServer(t, fb, secret)
	body := []byte(`{"event_id":"evt_renewal","event_type":"transaction.payment_failed","data":{"id":"txn_778","customer_id":"ctm_9","subscription_id":"sub_9"}}`)
	r := httptest.NewRequest("POST", "/v1/webhooks/paddle", strings.NewReader(string(body)))
	r.Header.Set("Paddle-Signature", paddleSig(secret, time.Now().Unix(), body))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("webhook = %d body %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "ignored") {
		t.Fatalf("renewal payment_failed dropped as uncorrelated: %s", w.Body)
	}
	if len(fb.applied) != 1 {
		t.Fatalf("applied = %d, want 1 (the event must reach org_billing)", len(fb.applied))
	}
	if fb.appliedOrgs[0] != "org_1" {
		t.Fatalf("correlated to org %q, want org_1", fb.appliedOrgs[0])
	}
	if fb.applied[0].Status != "past_due" {
		t.Fatalf("status = %q, want past_due", fb.applied[0].Status)
	}
}

// TestPaddleWebhook_CorrelatesByCustomerID is the second fallback: a
// subscription edited by support in the Paddle dashboard, or canceled through
// the customer portal, may reach us with neither custom_data nor a subscription
// id we recognise — but the customer id is stored too.
func TestPaddleWebhook_CorrelatesByCustomerID(t *testing.T) {
	const secret = "pdl_secret"
	fb := &fakeBilling{orgByPaddleID: map[string]string{"ctm_9": "org_2"}}
	s := billingServer(t, fb, secret)
	body := []byte(`{"event_id":"evt_portal_cancel","event_type":"subscription.canceled","data":{"id":"sub_unknown","customer_id":"ctm_9","status":"canceled"}}`)
	r := httptest.NewRequest("POST", "/v1/webhooks/paddle", strings.NewReader(string(body)))
	r.Header.Set("Paddle-Signature", paddleSig(secret, time.Now().Unix(), body))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "ignored") {
		t.Fatalf("portal cancellation dropped: %d %s", w.Code, w.Body)
	}
	if len(fb.appliedOrgs) != 1 || fb.appliedOrgs[0] != "org_2" {
		t.Fatalf("correlated to %v, want [org_2]", fb.appliedOrgs)
	}
}

// TestPaddleWebhook_UncorrelatedEventIsLogged: an event we genuinely cannot
// place must still be acked (Paddle has customer-level events that belong to no
// org), but it must leave a WARN line naming the event, so an operator can see
// the drop instead of inferring it from a subscription that never changed.
func TestPaddleWebhook_UncorrelatedEventIsLogged(t *testing.T) {
	const secret = "pdl_secret"
	var logged strings.Builder
	log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelWarn}))
	fb := &fakeBilling{}
	s := New(log, fakePinger{}, &fakeStore{}, &fakeDomain{}, Options{
		DevServiceToken:     testServiceToken,
		Billing:             fb,
		PaddleWebhookSecret: secret,
	})
	body := []byte(`{"event_id":"evt_orphan","event_type":"transaction.payment_failed","data":{"id":"txn_1","customer_id":"ctm_unknown","subscription_id":"sub_unknown"}}`)
	r := httptest.NewRequest("POST", "/v1/webhooks/paddle", strings.NewReader(string(body)))
	r.Header.Set("Paddle-Signature", paddleSig(secret, time.Now().Unix(), body))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("uncorrelated webhook = %d, want 200 ack", w.Code)
	}
	out := logged.String()
	if !strings.Contains(out, "evt_orphan") || !strings.Contains(out, "transaction.payment_failed") {
		t.Fatalf("dropped event left no WARN naming it: %q", out)
	}
}

// fakePaddleAPI is the outbound Paddle surface, so checkout can be exercised
// without pretending billing is unconfigured.
type fakePaddleAPI struct{ checkouts int }

func (f *fakePaddleAPI) CreateCheckout(_ context.Context, _ paddle.CreateTransactionInput) (paddle.Transaction, error) {
	f.checkouts++
	return paddle.Transaction{ID: "txn_new", CustomerID: "ctm_1", CheckoutURL: "https://pay.example/x"}, nil
}
func (f *fakePaddleAPI) CustomerPortalURL(context.Context, string) (string, error) {
	return "https://portal.example/x", nil
}
func (f *fakePaddleAPI) SetSubscriptionOrg(context.Context, string, string) error { return nil }

// TestBillingCheckout_RefusesWhenSubscriptionExists covers SIGMA-294: checkout
// never consulted GetBillingStatus, so an org that already had a subscription
// could create a second one. The reachable path was a PAUSE — the CP recorded
// it as "canceled", the dashboard therefore rendered a live Subscribe button as
// the only affordance, and completing that checkout left the org with two
// Paddle subscriptions, one of which org_billing cannot even track.
func TestBillingCheckout_RefusesWhenSubscriptionExists(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   store.BillingStatus
		wantCode int
	}{
		{"paused", store.BillingStatus{SubscriptionID: "sub_1", CustomerID: "ctm_1", Status: "paused"}, http.StatusConflict},
		{"active", store.BillingStatus{SubscriptionID: "sub_1", CustomerID: "ctm_1", Status: "active"}, http.StatusConflict},
		{"past_due", store.BillingStatus{SubscriptionID: "sub_1", CustomerID: "ctm_1", Status: "past_due"}, http.StatusConflict},
		// A genuinely canceled subscription is the one case where a new checkout
		// is the right answer, and an org that never subscribed must still work.
		{"canceled", store.BillingStatus{SubscriptionID: "sub_1", CustomerID: "ctm_1", Status: "canceled"}, http.StatusOK},
		{"none", store.BillingStatus{Status: "none"}, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fb := &fakeBilling{
				summary: store.BillingSummary{Connected: 8, Units: 8, BilledUnits: 8, BillableUnits: 5, Amount: 25},
				status:  tc.status,
			}
			fp := &fakePaddleAPI{}
			s := New(slog.Default(), fakePinger{}, &fakeStore{}, &fakeDomain{}, Options{
				DevServiceToken: testServiceToken,
				Billing:         fb,
				Paddle:          fp,
				PaddlePriceID:   "pri_1",
			})
			req := httptest.NewRequest("POST", "/v1/orgs/org_1/billing/checkout", nil)
			req.Header.Set("Authorization", "Bearer "+testServiceToken)
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			if rec.Code != tc.wantCode {
				t.Fatalf("checkout with a %s subscription = %d, want %d (body %s)",
					tc.name, rec.Code, tc.wantCode, rec.Body)
			}
			wantCheckouts := 0
			if tc.wantCode == http.StatusOK {
				wantCheckouts = 1
			}
			if fp.checkouts != wantCheckouts {
				t.Fatalf("created %d Paddle transactions, want %d", fp.checkouts, wantCheckouts)
			}
		})
	}
}

// TestPaddleSubStatus_PausedIsNotCanceled covers the other half of SIGMA-294:
// paused collapsed into "canceled" even though the same function handles
// subscription.resumed and therefore knows a pause is reversible. A customer who
// paused for a month saw "Canceled" and a Subscribe button.
func TestPaddleSubStatus_PausedIsNotCanceled(t *testing.T) {
	for _, tc := range []struct{ eventType, dataStatus, want string }{
		{"subscription.paused", "paused", "paused"},
		{"subscription.updated", "paused", "paused"},
		{"subscription.canceled", "canceled", "canceled"},
		{"subscription.updated", "canceled", "canceled"},
		{"subscription.resumed", "active", "active"},
		{"subscription.activated", "active", "active"},
		{"transaction.payment_failed", "", "past_due"},
	} {
		if got := paddleSubStatus(tc.eventType, tc.dataStatus); got != tc.want {
			t.Errorf("paddleSubStatus(%q, %q) = %q, want %q",
				tc.eventType, tc.dataStatus, got, tc.want)
		}
	}
}

// paddleSig mirrors paddle.VerifySignature's signing for the handler tests.
func paddleSig(secret string, ts int64, body []byte) string {
	return paddle.SignForTest(secret, ts, body)
}
