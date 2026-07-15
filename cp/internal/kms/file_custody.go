package kms

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// AuditSink receives every unwrap so it can be persisted outside this process
// (in dev: the CP audit log). A nil sink is allowed — events are still kept
// in-memory and returned by AuditEvents.
type AuditSink func(ctx context.Context, ev AuditEvent) error

// FileCustody is the DEV key custody: a single AES-256-GCM master key read
// from (or generated into) a 0600 file. It is NOT for production — the master
// key sits on the same host as the ciphertext, so it provides envelope
// hygiene and the unwrap audit trail, not a real trust boundary.
type FileCustody struct {
	aead  cipher.AEAD
	sink  AuditSink
	mu    sync.Mutex
	audit []AuditEvent
}

// LoadOrCreateFileCustody reads the master key at keyPath, generating a fresh
// 32-byte key (0600) on first use.
func LoadOrCreateFileCustody(keyPath string, sink AuditSink) (*FileCustody, error) {
	key, err := loadOrCreateKey(keyPath)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &FileCustody{aead: aead, sink: sink}, nil
}

func loadOrCreateKey(keyPath string) ([]byte, error) {
	b, err := os.ReadFile(keyPath)
	switch {
	case err == nil:
		key, decErr := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if decErr != nil {
			return nil, fmt.Errorf("corrupt kms key %s: %w", keyPath, decErr)
		}
		if len(key) != 32 {
			return nil, fmt.Errorf("kms key %s must be 32 bytes, got %d", keyPath, len(key))
		}
		return key, nil
	case errors.Is(err, fs.ErrNotExist):
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
			return nil, err
		}
		enc := base64.StdEncoding.EncodeToString(key)
		if err := os.WriteFile(keyPath, []byte(enc+"\n"), 0o600); err != nil {
			return nil, err
		}
		return key, nil
	default:
		return nil, err
	}
}

func (c *FileCustody) Wrap(_ context.Context, _ string, plaintext []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	// Envelope = nonce || ciphertext(+tag).
	return c.aead.Seal(nonce, nonce, plaintext, nil), nil
}

func (c *FileCustody) Unwrap(ctx context.Context, purpose string, envelope []byte) ([]byte, error) {
	ns := c.aead.NonceSize()
	if len(envelope) < ns {
		return nil, errors.New("kms: ciphertext too short")
	}
	nonce, ct := envelope[:ns], envelope[ns:]
	plaintext, err := c.aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("kms: unwrap failed: %w", err)
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

func (c *FileCustody) AuditEvents(_ context.Context) ([]AuditEvent, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]AuditEvent, len(c.audit))
	copy(out, c.audit)
	return out, nil
}
