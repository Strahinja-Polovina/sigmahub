package api

import (
	"context"
	"log/slog"
	"encoding/json"
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
	applied   []store.BillingStatus
	seen      map[string]bool
	seenCalls int
}

func (f *fakeBilling) BillingSummaryForOrg(_ context.Context, orgID string, _ time.Time, configured bool) (store.BillingSummary, error) {
	s := f.summary
	s.Configured = configured
	return s, nil
}
func (f *fakeBilling) GetBillingStatus(context.Context, string) (store.BillingStatus, error) {
	return store.BillingStatus{Status: "none"}, nil
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
	fb := &fakeBilling{summary: store.BillingSummary{Connected: 4, BillableServers: 1, Amount: 5}}
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

// paddleSig mirrors paddle.VerifySignature's signing for the handler tests.
func paddleSig(secret string, ts int64, body []byte) string {
	return paddle.SignForTest(secret, ts, body)
}
