package store

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/kms"
)

const tokenPepperName = "token_pepper"

// AuditUnwrapSink returns a kms.AuditSink that records every unwrap in the CP
// audit log. KMS operations are system-scoped, so they land under org 'system'.
func (s *Store) AuditUnwrapSink() kms.AuditSink {
	return func(ctx context.Context, ev kms.AuditEvent) error {
		_, err := s.Pool.Exec(ctx, `
			INSERT INTO cp_audit_log (org_id, actor, action, target)
			VALUES ('system', 'kms', 'Key unwrapped', $1)`, ev.Purpose)
		return err
	}
}

// LoadTokenPepper returns the HMAC pepper used to key every token hash,
// creating and wrapping it on first boot. The value is only ever persisted
// wrapped; unwrapping it here emits one audit event via the custody's sink.
func (s *Store) LoadTokenPepper(ctx context.Context, custody kms.KeyCustody) ([]byte, error) {
	var wrapped []byte
	err := s.Pool.QueryRow(ctx,
		`SELECT wrapped FROM cp_secrets WHERE name = $1`, tokenPepperName).Scan(&wrapped)

	if errors.Is(err, pgx.ErrNoRows) {
		pepper := make([]byte, 32)
		if _, err := rand.Read(pepper); err != nil {
			return nil, err
		}
		wrapped, err = custody.Wrap(ctx, tokenPepperName, pepper)
		if err != nil {
			return nil, fmt.Errorf("wrap token pepper: %w", err)
		}
		// ON CONFLICT DO NOTHING makes concurrent first-boots converge on one
		// pepper; we re-read the winner below rather than trust our own insert.
		if _, err := s.Pool.Exec(ctx, `
			INSERT INTO cp_secrets (name, wrapped) VALUES ($1, $2)
			ON CONFLICT (name) DO NOTHING`, tokenPepperName, wrapped); err != nil {
			return nil, fmt.Errorf("insert token pepper: %w", err)
		}
		if err := s.Pool.QueryRow(ctx,
			`SELECT wrapped FROM cp_secrets WHERE name = $1`, tokenPepperName).Scan(&wrapped); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}

	pepper, err := custody.Unwrap(ctx, tokenPepperName, wrapped)
	if err != nil {
		return nil, fmt.Errorf("unwrap token pepper: %w", err)
	}
	return pepper, nil
}

// SetPepper installs the HMAC pepper used by token hashing. Must be called
// before any token is issued or authenticated.
func (s *Store) SetPepper(pepper []byte) { s.pepper = pepper }
