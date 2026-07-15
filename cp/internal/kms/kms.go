// Package kms is SigmaHub's key-custody boundary. The control plane never
// stores long-lived secrets (the token pepper, and later tenant data-keys) in
// the clear: they live wrapped, and are unwrapped through a KeyCustody whose
// every Unwrap is audited. This package is the interface plus a dev-grade,
// file-anchored implementation; the production path (external KMS/HSM with an
// out-of-band unwrap audit trail and quorum on bulk decrypt) implements the
// same interface. See KB: "Key custody hardening (P0-9)".
package kms

import "context"

// AuditEvent is one custody operation worth recording. In the dev stub these
// are mirrored into the CP audit log via the sink; the production custody
// anchors them outside the primary infrastructure.
type AuditEvent struct {
	Action  string // e.g. "kms.unwrap"
	Purpose string // caller-supplied label, e.g. "token_pepper"
}

// KeyCustody wraps/unwraps secrets and exposes the audit trail of unwraps.
// Every Unwrap MUST emit an AuditEvent — that invariant is the whole point of
// routing secret access through this boundary.
type KeyCustody interface {
	// Wrap encrypts plaintext for storage at rest. purpose is a non-secret
	// label recorded on later unwraps.
	Wrap(ctx context.Context, purpose string, plaintext []byte) ([]byte, error)
	// Unwrap decrypts and, as a side effect, emits an AuditEvent.
	Unwrap(ctx context.Context, purpose string, ciphertext []byte) ([]byte, error)
	// AuditEvents returns the unwrap events observed by this custody instance.
	AuditEvents(ctx context.Context) ([]AuditEvent, error)
}
