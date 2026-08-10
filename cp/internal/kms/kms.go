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

// auditRingCap bounds the in-memory unwrap trail every custody keeps
// (SIGMA-319).
//
// That slice used to be appended to forever. Unwrap is called from background
// loops — the fleet resync unwraps a cluster's join token for every node it
// re-renders, every 60 seconds — so the trail grew in proportion to process
// UPTIME rather than to anything a human did, and a long-lived control plane
// leaked memory it could never reclaim. The durable record is the sink (which
// writes cp_audit_log); this in-memory copy is a debugging convenience, so the
// right size for it is "the most recent N", not "all of them".
const auditRingCap = 1024

// appendAudit adds ev to a bounded in-memory trail, dropping the oldest event
// once the cap is reached. Callers must hold their own lock.
func appendAudit(ring []AuditEvent, ev AuditEvent) []AuditEvent {
	if len(ring) >= auditRingCap {
		// Shift down by one rather than reallocating, so steady state does no
		// allocation at all.
		copy(ring, ring[len(ring)-auditRingCap+1:])
		ring = ring[:auditRingCap-1]
	}
	return append(ring, ev)
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
