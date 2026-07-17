package paddle

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"
)

func sign(secret, ts string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + ":"))
	mac.Write(body)
	return fmt.Sprintf("ts=%s;h1=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}

func TestVerifySignature(t *testing.T) {
	secret := "pdl_ntfset_secret"
	body := []byte(`{"event_id":"evt_1","event_type":"subscription.updated"}`)
	now := time.Unix(1_700_000_000, 0)
	ts := "1700000000"

	// Valid signature within skew.
	if !VerifySignature(secret, body, sign(secret, ts, body), now, 5*time.Minute) {
		t.Fatal("valid signature must verify")
	}
	// Wrong secret.
	if VerifySignature("other", body, sign(secret, ts, body), now, 5*time.Minute) {
		t.Fatal("wrong secret must not verify")
	}
	// Tampered body.
	if VerifySignature(secret, []byte(`{"event_id":"evt_2"}`), sign(secret, ts, body), now, 5*time.Minute) {
		t.Fatal("tampered body must not verify")
	}
	// Stale timestamp (beyond skew) — replay guard.
	old := "1699000000"
	if VerifySignature(secret, body, sign(secret, old, body), now, 5*time.Minute) {
		t.Fatal("stale timestamp must be rejected")
	}
	// Empty secret never verifies.
	if VerifySignature("", body, sign(secret, ts, body), now, 5*time.Minute) {
		t.Fatal("empty secret must not verify")
	}
	// Malformed header.
	if VerifySignature(secret, body, "garbage", now, 5*time.Minute) {
		t.Fatal("malformed header must not verify")
	}
}

func TestNewClientOffWithoutKey(t *testing.T) {
	if NewClient("sandbox", "") != nil {
		t.Fatal("empty API key must yield a nil client (billing off)")
	}
	c := NewClient("sandbox", "key")
	if c == nil || c.APIBase != SandboxAPIBase {
		t.Fatalf("sandbox client = %+v", c)
	}
	if NewClient("production", "key").APIBase != ProductionAPIBase {
		t.Fatal("production base wrong")
	}
}
