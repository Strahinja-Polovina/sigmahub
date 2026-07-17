package kms

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeVault emulates the two transit endpoints the custody uses: encrypt
// prefixes the plaintext with a version marker, decrypt strips it. Tokens and
// key names are checked so a mis-wired custody fails the way real Vault would.
func fakeVault(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	requireAuth := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("X-Vault-Token") != "test-token" {
			w.WriteHeader(http.StatusForbidden)
			return false
		}
		return true
	}
	mux.HandleFunc("GET /v1/transit/keys/sigmahub-cp", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		_, _ = w.Write([]byte(`{"data":{"name":"sigmahub-cp"}}`))
	})
	mux.HandleFunc("POST /v1/transit/encrypt/sigmahub-cp", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		var body struct {
			Plaintext string `json:"plaintext"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]string{"ciphertext": "vault:v1:" + body.Plaintext},
		})
	})
	mux.HandleFunc("POST /v1/transit/decrypt/sigmahub-cp", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		var body struct {
			Ciphertext string `json:"ciphertext"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !strings.HasPrefix(body.Ciphertext, "vault:v1:") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"errors":["invalid ciphertext"]}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]string{"plaintext": strings.TrimPrefix(body.Ciphertext, "vault:v1:")},
		})
	})
	return httptest.NewServer(mux)
}

func TestVaultCustodyRoundTripAndAudit(t *testing.T) {
	srv := fakeVault(t)
	defer srv.Close()

	var sunk []AuditEvent
	custody, err := NewVaultCustody(context.Background(), VaultConfig{
		Addr: srv.URL, Token: "test-token",
	}, func(_ context.Context, ev AuditEvent) error {
		sunk = append(sunk, ev)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	secret := []byte("token-pepper-material")
	envelope, err := custody.Wrap(context.Background(), "token_pepper", secret)
	if err != nil {
		t.Fatal(err)
	}
	// The stored envelope is Vault's versioned ciphertext, never the plaintext.
	if !strings.HasPrefix(string(envelope), "vault:v1:") {
		t.Fatalf("envelope = %q", envelope)
	}
	if strings.Contains(string(envelope), string(secret)) {
		t.Fatal("envelope contains raw plaintext")
	}
	if base64.StdEncoding.EncodeToString(secret) == string(envelope) {
		t.Fatal("envelope is bare base64 of the plaintext")
	}

	got, err := custody.Unwrap(context.Background(), "token_pepper", envelope)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(secret) {
		t.Fatalf("round trip = %q, want %q", got, secret)
	}
	// The unwrap invariant: exactly one audit event, mirrored into the sink.
	events, _ := custody.AuditEvents(context.Background())
	if len(events) != 1 || events[0].Purpose != "token_pepper" || len(sunk) != 1 {
		t.Fatalf("audit = %+v sink = %+v", events, sunk)
	}
}

func TestVaultCustodyFailsBootOnBadToken(t *testing.T) {
	srv := fakeVault(t)
	defer srv.Close()
	if _, err := NewVaultCustody(context.Background(), VaultConfig{
		Addr: srv.URL, Token: "wrong",
	}, nil); err == nil {
		t.Fatal("bad token must fail construction, not the first unwrap")
	}
}

func TestVaultCustodyDecryptErrorPropagates(t *testing.T) {
	srv := fakeVault(t)
	defer srv.Close()
	custody, err := NewVaultCustody(context.Background(), VaultConfig{Addr: srv.URL, Token: "test-token"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := custody.Unwrap(context.Background(), "x", []byte("garbage")); err == nil {
		t.Fatal("invalid ciphertext must error")
	}
	// A failed unwrap must not record a successful audit event.
	events, _ := custody.AuditEvents(context.Background())
	if len(events) != 0 {
		t.Fatalf("audit after failure = %+v", events)
	}
}
