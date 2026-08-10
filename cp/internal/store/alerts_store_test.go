package store

// Alert-channel destination validation (SIGMA-259).
//
// An alert channel is the ONLY egress destination a tenant chooses for the
// control plane. The CP runs inside the operator's private network, next to
// Postgres, VictoriaMetrics and Loki, and it will POST to whatever a channel
// names — so "any URL the tenant likes" is a request-forgery primitive handed
// to every self-service signup, and the delivery error carries part of the
// response back out.

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestValidateChannelConfig_RejectsInternalDestinations(t *testing.T) {
	// The cloud metadata service, the CP's own loopback, an RFC1918 neighbour,
	// and a name that resolves to loopback with a non-web port. Each is a real
	// thing an attacker asks for: IAM credentials, an unauthenticated admin API
	// on the CP host, an internal write endpoint, a search index.
	destinations := []string{
		"http://169.254.169.254/latest/meta-data/iam/security-credentials/",
		"http://127.0.0.1:9200/_cat/indices",
		"http://10.0.0.1/",
		"http://localhost:9200/_cat/indices",
	}
	for _, dest := range destinations {
		// webhook: the destination is config.url.
		cfg, err := json.Marshal(map[string]string{"url": dest})
		if err != nil {
			t.Fatal(err)
		}
		if err := validateChannelConfig("webhook", cfg, ""); err == nil {
			t.Errorf("webhook to %s was accepted — the CP would POST to it", dest)
		}
		// slack: the destination IS the secret, and it had no validation at all.
		if err := validateChannelConfig("slack", json.RawMessage(`{}`), dest); err == nil {
			t.Errorf("slack channel pointed at %s was accepted", dest)
		}
	}

	// A non-web port is refused even on a public address: an alert channel
	// delivers over HTTP(S), and everything else is somebody's service port.
	if err := validateChannelConfig("webhook",
		json.RawMessage(`{"url":"http://93.184.216.34:8480/insert/0/prometheus"}`), ""); err == nil {
		t.Error("a webhook to a non-80/443 port was accepted")
	}

	// And the legitimate shapes still work. Literal public IP so the check does
	// not depend on DNS being available to the test runner.
	if err := validateChannelConfig("webhook",
		json.RawMessage(`{"url":"https://93.184.216.34/hooks/sigmahub"}`), ""); err != nil {
		t.Errorf("a public https webhook must be accepted: %v", err)
	}
	if err := validateChannelConfig("slack", json.RawMessage(`{}`),
		"https://hooks.slack.com/services/T000/B000/xxxx"); err != nil {
		t.Errorf("a real Slack incoming-webhook URL must be accepted: %v", err)
	}
	// Anything that is not Slack's incoming-webhook host is not a slack channel,
	// whatever it resolves to.
	if err := validateChannelConfig("slack", json.RawMessage(`{}`),
		"https://hooks.slack.com.evil.example/T000"); err == nil {
		t.Error("a look-alike slack host was accepted")
	}

	// The refusal has to be a 400-shaped error the dashboard can render, not a
	// generic failure.
	var inv ErrInvalid
	err := validateChannelConfig("webhook", json.RawMessage(`{"url":"http://127.0.0.1/"}`), "")
	if !errors.As(err, &inv) || !strings.Contains(strings.ToLower(inv.Msg), "internal") {
		t.Errorf("refusal = %#v, want an ErrInvalid naming the problem", err)
	}
}
