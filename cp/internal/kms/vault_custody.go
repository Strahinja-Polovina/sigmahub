package kms

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// VaultCustody is the production key custody: wrap/unwrap ride HashiCorp
// Vault's transit engine, so the master key material never exists on the CP
// host at all — a real trust boundary, unlike FileCustody. Every Unwrap still
// emits the local AuditEvent (the P0-9 invariant) AND leaves Vault's own
// server-side audit trail, giving the out-of-band anchor the design doc calls
// for. Implemented with the standard library only (one POST per operation).
type VaultCustody struct {
	addr      string // e.g. https://vault.internal:8200
	token     string
	key       string // transit key name
	namespace string // optional Vault Enterprise namespace
	http      *http.Client

	sink  AuditSink
	mu    sync.Mutex
	audit []AuditEvent
}

// VaultConfig locates the transit engine and key.
type VaultConfig struct {
	Addr      string
	Token     string
	TransitKey string
	Namespace  string
}

// NewVaultCustody validates the config and verifies connectivity by asking
// Vault for the transit key (failing boot beats failing the first unwrap).
func NewVaultCustody(ctx context.Context, cfg VaultConfig, sink AuditSink) (*VaultCustody, error) {
	if cfg.Addr == "" || cfg.Token == "" {
		return nil, fmt.Errorf("vault custody: CP_VAULT_ADDR and CP_VAULT_TOKEN are required")
	}
	key := cfg.TransitKey
	if key == "" {
		key = "sigmahub-cp"
	}
	c := &VaultCustody{
		addr:      strings.TrimSuffix(cfg.Addr, "/"),
		token:     cfg.Token,
		key:       key,
		namespace: cfg.Namespace,
		http:      &http.Client{Timeout: 10 * time.Second},
		sink:      sink,
	}
	if err := c.ping(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// ping reads the transit key metadata so a bad address/token/key fails boot.
func (c *VaultCustody) ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.addr+"/v1/transit/keys/"+c.key, nil)
	if err != nil {
		return err
	}
	c.headers(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("vault custody: unreachable: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("vault custody: transit key %q not readable (HTTP %d) — create it with `vault write -f transit/keys/%s`", c.key, resp.StatusCode, c.key)
	}
	return nil
}

func (c *VaultCustody) headers(req *http.Request) {
	req.Header.Set("X-Vault-Token", c.token)
	if c.namespace != "" {
		req.Header.Set("X-Vault-Namespace", c.namespace)
	}
}

// transit posts one encrypt/decrypt operation and returns the named field
// from Vault's data envelope.
func (c *VaultCustody) transit(ctx context.Context, op string, body map[string]string, field string) (string, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.addr+"/v1/transit/"+op+"/"+c.key, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	c.headers(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("vault custody: %s: %w", op, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("vault custody: %s failed (HTTP %d): %s", op, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	// Decode the data envelope leniently: real Vault transit encrypt responses
	// include a numeric key_version alongside the string ciphertext, so a
	// map[string]string here fails to unmarshal and breaks every Wrap
	// (SIGMA-140). Use RawMessage and read only the one string field we need.
	var out struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("vault custody: %s: bad response: %w", op, err)
	}
	rawField, ok := out.Data[field]
	if !ok {
		return "", fmt.Errorf("vault custody: %s: response missing %q", op, field)
	}
	var v string
	if err := json.Unmarshal(rawField, &v); err != nil {
		return "", fmt.Errorf("vault custody: %s: field %q is not a string: %w", op, field, err)
	}
	if v == "" {
		return "", fmt.Errorf("vault custody: %s: response missing %q", op, field)
	}
	return v, nil
}

// Wrap encrypts plaintext under the transit key; the stored envelope is
// Vault's versioned ciphertext string ("vault:vN:..."), so transit key
// rotation transparently applies to new wraps while old envelopes stay
// decryptable.
//
// purpose is intentionally NOT bound as a transit `context` here (SIGMA-80):
// transit convergent/derived encryption requires the key to be created with
// `derived=true`, and sending a context to a non-derived key errors — so
// binding it unconditionally would break Wrap on every existing deployment and
// leave already-stored envelopes (encrypted without context) undecryptable.
// The cross-row swap this would defend against is already defeated upstream by
// secretAAD; binding at the Vault layer is a derived-key provisioning change
// tracked separately, not a silent behaviour change in a bug-fix.
func (c *VaultCustody) Wrap(ctx context.Context, _ string, plaintext []byte) ([]byte, error) {
	ct, err := c.transit(ctx, "encrypt", map[string]string{
		"plaintext": base64.StdEncoding.EncodeToString(plaintext),
	}, "ciphertext")
	if err != nil {
		return nil, err
	}
	return []byte(ct), nil
}

func (c *VaultCustody) Unwrap(ctx context.Context, purpose string, envelope []byte) ([]byte, error) {
	b64, err := c.transit(ctx, "decrypt", map[string]string{
		"ciphertext": string(envelope),
	}, "plaintext")
	if err != nil {
		return nil, err
	}
	plaintext, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("vault custody: bad plaintext encoding: %w", err)
	}

	ev := AuditEvent{Action: "kms.unwrap", Purpose: purpose}
	c.mu.Lock()
	c.audit = append(c.audit, ev)
	c.mu.Unlock()
	if c.sink != nil {
		if err := c.sink(ctx, ev); err != nil {
			return nil, fmt.Errorf("kms: audit sink: %w", err)
		}
	}
	return plaintext, nil
}

func (c *VaultCustody) AuditEvents(_ context.Context) ([]AuditEvent, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]AuditEvent, len(c.audit))
	copy(out, c.audit)
	return out, nil
}
