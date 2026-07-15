package kms

import (
	"context"
	"path/filepath"
	"testing"
)

func TestWrapUnwrapRoundTrip(t *testing.T) {
	c, err := LoadOrCreateFileCustody(filepath.Join(t.TempDir(), "master.key"), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	secret := []byte("the token pepper")

	wrapped, err := c.Wrap(ctx, "token_pepper", secret)
	if err != nil {
		t.Fatal(err)
	}
	if string(wrapped) == string(secret) {
		t.Fatal("wrapped output equals plaintext")
	}
	got, err := c.Unwrap(ctx, "token_pepper", wrapped)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(secret) {
		t.Fatalf("round-trip mismatch: %q != %q", got, secret)
	}
}

func TestEveryUnwrapEmitsAudit(t *testing.T) {
	var sunk []AuditEvent
	sink := func(_ context.Context, ev AuditEvent) error {
		sunk = append(sunk, ev)
		return nil
	}
	c, err := LoadOrCreateFileCustody(filepath.Join(t.TempDir(), "master.key"), sink)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	wrapped, _ := c.Wrap(ctx, "token_pepper", []byte("x"))

	for i := 0; i < 3; i++ {
		if _, err := c.Unwrap(ctx, "token_pepper", wrapped); err != nil {
			t.Fatal(err)
		}
	}

	events, err := c.AuditEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || len(sunk) != 3 {
		t.Fatalf("want 3 audit events (in-memory=%d, sink=%d)", len(events), len(sunk))
	}
	for _, ev := range events {
		if ev.Action != "kms.unwrap" || ev.Purpose != "token_pepper" {
			t.Fatalf("unexpected event: %+v", ev)
		}
	}
}

func TestKeyStableAcrossReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")
	ctx := context.Background()

	c1, err := LoadOrCreateFileCustody(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	wrapped, _ := c1.Wrap(ctx, "p", []byte("secret"))

	// A fresh custody loading the same key file must unwrap prior ciphertext.
	c2, err := LoadOrCreateFileCustody(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c2.Unwrap(ctx, "p", wrapped)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "secret" {
		t.Fatalf("reloaded custody unwrap = %q, want 'secret'", got)
	}
}

func TestUnwrapRejectsTampered(t *testing.T) {
	c, err := LoadOrCreateFileCustody(filepath.Join(t.TempDir(), "master.key"), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	wrapped, _ := c.Wrap(ctx, "p", []byte("secret"))
	wrapped[len(wrapped)-1] ^= 0xff // flip a ciphertext bit

	if _, err := c.Unwrap(ctx, "p", wrapped); err == nil {
		t.Fatal("tampered ciphertext unwrapped without error")
	}
}
