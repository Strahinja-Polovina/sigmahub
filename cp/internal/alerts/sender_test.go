package alerts

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/smtp"
	"strings"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// testSender is NewSender with the SIGMA-259 destination guard switched off.
// Every delivery test here posts to an httptest server, which listens on
// 127.0.0.1 — precisely the address the guard exists to refuse. The guard
// itself is covered by TestSenderRefusesInternalDestinations below.
func testSender() *Sender {
	s := NewSender()
	s.allowInternalDestinations = true
	return s
}

func TestSendSlack(t *testing.T) {
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
	}))
	defer srv.Close()

	s := testSender()
	ch := store.AlertChannelSend{Kind: "slack", Secret: srv.URL}
	if err := s.Send(context.Background(), ch, "deploy_failed", "Deploy failed", "boom"); err != nil {
		t.Fatal(err)
	}
	if got["text"] != "*Deploy failed*\nboom" {
		t.Fatalf("slack payload = %q", got["text"])
	}

	// The webhook URL is the channel secret: a failing send must not leak it.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "channel_not_found for "+srv.URL, http.StatusNotFound)
	}))
	defer srv2.Close()
	err := s.Send(context.Background(), store.AlertChannelSend{Kind: "slack", Secret: srv2.URL}, "x", "t", "b")
	if err == nil {
		t.Fatal("non-2xx must error")
	}
	if strings.Contains(err.Error(), srv2.URL) {
		t.Fatalf("error leaks the secret webhook URL: %v", err)
	}
}

func TestSendWebhookSignsPayload(t *testing.T) {
	const signingKey = "topsecret"
	var gotBody []byte
	var gotSig string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotSig = r.Header.Get("X-Sigmahub-Signature-256")
	}))
	defer srv.Close()

	s := testSender()
	ch := store.AlertChannelSend{
		Kind:   "webhook",
		Config: []byte(`{"url":"` + srv.URL + `"}`),
		Secret: signingKey,
	}
	if err := s.Send(context.Background(), ch, "backup_failed", "Backup failed", "details"); err != nil {
		t.Fatal(err)
	}

	var p WebhookPayload
	if err := json.Unmarshal(gotBody, &p); err != nil {
		t.Fatal(err)
	}
	if p.Event != "backup_failed" || p.Title != "Backup failed" || p.Body != "details" {
		t.Fatalf("payload = %+v", p)
	}
	mac := hmac.New(sha256.New, []byte(signingKey))
	mac.Write(gotBody)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if gotSig != want {
		t.Fatalf("signature = %q, want %q (over exact body)", gotSig, want)
	}
}

func TestSendTelegram(t *testing.T) {
	var gotPath string
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
	}))
	defer srv.Close()

	s := testSender()
	s.telegramBase = srv.URL
	ch := store.AlertChannelSend{
		Kind:   "telegram",
		Config: []byte(`{"chatId":"-100123"}`),
		Secret: "bot-token",
	}
	if err := s.Send(context.Background(), ch, "server_unreachable", "Server down", "srv-1"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/botbot-token/sendMessage" {
		t.Fatalf("path = %q", gotPath)
	}
	if got["chat_id"] != "-100123" || got["text"] != "Server down\nsrv-1" {
		t.Fatalf("telegram payload = %v", got)
	}

	// Missing chat id is a config error, not a silent no-op.
	bad := store.AlertChannelSend{Kind: "telegram", Config: []byte(`{}`), Secret: "tok"}
	if err := s.Send(context.Background(), bad, "x", "t", "b"); err == nil {
		t.Fatal("telegram without chatId must error")
	}
}

// SIGMA-259: the send path enforces the same destination rule the store does at
// create time, for both kinds whose destination the tenant chooses.
//
// Create-time validation alone would be a half-fix: every channel created
// before that check existed is still in the table, and a hostname that
// validated as public can answer with 10.0.0.5 an hour later.
func TestSenderRefusesInternalDestinations(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
	}))
	defer srv.Close()

	// A production sender: the guard is on. srv.URL is 127.0.0.1 — stand-in for
	// the Elasticsearch, VictoriaMetrics or metadata endpoint an attacker names.
	s := NewSender()
	for _, ch := range []store.AlertChannelSend{
		{Kind: "slack", Secret: srv.URL},
		{Kind: "webhook", Config: []byte(`{"url":"` + srv.URL + `"}`)},
	} {
		err := s.Send(context.Background(), ch, "deploy_failed", "t", "b")
		if err == nil {
			t.Fatalf("%s channel to %s was delivered", ch.Kind, srv.URL)
		}
		if !strings.Contains(err.Error(), "internal address") && !strings.Contains(err.Error(), "port must be") {
			t.Fatalf("%s refusal = %v, want a destination refusal", ch.Kind, err)
		}
	}
	if hits != 0 {
		t.Fatalf("the control plane made %d requests to the internal endpoint", hits)
	}

	// And a redirect cannot walk past the check: the client re-applies it on
	// every hop, so a public URL answering 302 http://169.254.169.254/ stops.
	req, _ := http.NewRequest("GET", "http://169.254.169.254/latest/meta-data/", nil)
	if err := s.HTTP.CheckRedirect(req, nil); err == nil {
		t.Fatal("a redirect to the metadata service was allowed")
	}
}

// A non-2xx from a tenant-chosen receiver must not quote the receiver's body
// back: that is what turned an alert channel into a read-back channel for
// whatever the CP was pointed at.
func TestWebhookErrorDoesNotEchoResponseBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "AccessKeyId: AKIAIOSFODNN7EXAMPLE", http.StatusForbidden)
	}))
	defer srv.Close()

	s := testSender()
	err := s.Send(context.Background(), store.AlertChannelSend{
		Kind: "webhook", Config: []byte(`{"url":"` + srv.URL + `"}`),
	}, "deploy_failed", "t", "b")
	if err == nil {
		t.Fatal("a 403 must be an error")
	}
	if strings.Contains(err.Error(), "AKIAIOSFODNN7EXAMPLE") {
		t.Fatalf("the upstream body rode the delivery error: %v", err)
	}
	if !strings.Contains(err.Error(), "403") {
		t.Fatalf("the status is the actionable part and must stay: %v", err)
	}

	// Same rule for slack, whose destination is equally tenant-chosen.
	err = s.Send(context.Background(), store.AlertChannelSend{Kind: "slack", Secret: srv.URL},
		"deploy_failed", "t", "b")
	if err == nil || strings.Contains(err.Error(), "AKIAIOSFODNN7EXAMPLE") {
		t.Fatalf("slack delivery error = %v", err)
	}
}

func TestSendEmail(t *testing.T) {
	var gotAddr, gotFrom string
	var gotTo []string
	var gotMsg []byte
	s := testSender()
	s.sendMail = func(addr string, _ smtp.Auth, from string, to []string, msg []byte) error {
		gotAddr, gotFrom, gotTo, gotMsg = addr, from, to, msg
		return nil
	}
	ch := store.AlertChannelSend{
		Kind:   "email",
		Config: []byte(`{"host":"smtp.example.com","from":"alerts@example.com","to":["ops@example.com"],"username":"u"}`),
		Secret: "smtp-pass",
	}
	if err := s.Send(context.Background(), ch, "verify_failed", "Verify failed\r\nX-Evil: 1", "body"); err != nil {
		t.Fatal(err)
	}
	if gotAddr != "smtp.example.com:587" || gotFrom != "alerts@example.com" || len(gotTo) != 1 {
		t.Fatalf("smtp call = %q %q %v", gotAddr, gotFrom, gotTo)
	}
	msg := string(gotMsg)
	headers := strings.SplitN(msg, "\r\n\r\n", 2)[0]
	if strings.Contains(headers, "\r\nX-Evil") {
		t.Fatalf("crafted title injected a header: %q", headers)
	}
	if !strings.Contains(headers, "Subject: [SigmaHub] Verify failed") {
		t.Fatalf("subject missing: %q", headers)
	}

	// SMTP failures must not leak the password into the stored error.
	s.sendMail = func(string, smtp.Auth, string, []string, []byte) error {
		return fmt.Errorf("535 auth failed for smtp-pass")
	}
	err := s.Send(context.Background(), ch, "x", "t", "b")
	if err == nil || strings.Contains(err.Error(), "smtp-pass") {
		t.Fatalf("error must exist and must not leak the password: %v", err)
	}
}
