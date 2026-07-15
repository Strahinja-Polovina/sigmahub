package store

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// RotateKEK re-wraps every one of an org's DEKs under the current custody key
// WITHOUT re-encrypting any secret data (the DEK plaintext is unchanged, so all
// ciphertexts still decrypt). Each wrapped-DEK's wrap_version advances. This is
// the cheap rotation: turning the KEK does not touch tenant data. Returns the
// number of DEKs re-wrapped. Audited.
func (s *Store) RotateKEK(ctx context.Context, orgID, actor string) (int, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `SELECT id, wrapped_dek FROM org_deks WHERE org_id = $1`, orgID)
	if err != nil {
		return 0, err
	}
	type dekRow struct {
		id      string
		wrapped []byte
	}
	var deks []dekRow
	for rows.Next() {
		var d dekRow
		if err := rows.Scan(&d.id, &d.wrapped); err != nil {
			rows.Close()
			return 0, err
		}
		deks = append(deks, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for _, d := range deks {
		plain, err := s.custody.Unwrap(ctx, "org_dek:"+orgID, d.wrapped)
		if err != nil {
			return 0, fmt.Errorf("unwrap for rewrap: %w", err)
		}
		rewrapped, err := s.custody.Wrap(ctx, "org_dek:"+orgID, plain)
		if err != nil {
			return 0, fmt.Errorf("rewrap: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE org_deks SET wrapped_dek = $2, wrap_version = wrap_version + 1 WHERE id = $1`,
			d.id, rewrapped); err != nil {
			return 0, err
		}
	}
	if len(deks) > 0 {
		if err := auditTx(ctx, tx, orgID, actor, "KEK rotated", fmt.Sprintf("%d DEKs re-wrapped", len(deks))); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(deks), nil
}

// RotateDEK starts a DEK rotation: it deactivates the org's current DEK and
// creates a fresh active one, so NEW writes use the new key. Existing secrets
// stay on the old (now-inactive, not-yet-retired) DEK until ReencryptSecrets
// lazily migrates them. Audited.
func (s *Store) RotateDEK(ctx context.Context, orgID, actor string) (string, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Ensure there is an active DEK to rotate from (creates one if the org has
	// never held a secret — a no-op rotation then).
	if _, _, err := s.activeDEKTx(ctx, tx, orgID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE org_deks SET active = FALSE WHERE org_id = $1 AND active`, orgID); err != nil {
		return "", err
	}
	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		return "", err
	}
	wrapped, err := s.custody.Wrap(ctx, "org_dek:"+orgID, dek)
	if err != nil {
		return "", err
	}
	dekID := newID("dek")
	if _, err := tx.Exec(ctx,
		`INSERT INTO org_deks (id, org_id, wrapped_dek, active) VALUES ($1, $2, $3, TRUE)`, dekID, orgID, wrapped); err != nil {
		return "", err
	}
	if err := auditTx(ctx, tx, orgID, actor, "DEK rotation started", dekID); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	s.cacheDEK(dekID, dek)
	return dekID, nil
}

// ReencryptSecrets migrates any secrets still on a non-active DEK onto the org's
// active DEK (decrypt with the old key, re-encrypt with the new — AAD is
// identity-bound so it is unchanged), then retires any inactive DEK that no
// longer backs a secret. This is the lazy re-encrypt a background job drives;
// it is idempotent and safe to run repeatedly. Returns rows re-encrypted.
func (s *Store) ReencryptSecrets(ctx context.Context, orgID string) (int, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var activeDEKID string
	err = tx.QueryRow(ctx, `SELECT id FROM org_deks WHERE org_id = $1 AND active`, orgID).Scan(&activeDEKID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil // nothing to do
	}
	if err != nil {
		return 0, err
	}
	activeDEK, err := s.dekPlaintext(ctx, tx, activeDEKID)
	if err != nil {
		return 0, err
	}

	rows, err := tx.Query(ctx, `
		SELECT id, project_id, ciphertext, nonce, dek_id
		  FROM secrets WHERE org_id = $1 AND dek_id <> $2`, orgID, activeDEKID)
	if err != nil {
		return 0, err
	}
	type stale struct {
		id, projectID, dekID string
		ct, nonce            []byte
	}
	var stales []stale
	for rows.Next() {
		var st stale
		if err := rows.Scan(&st.id, &st.projectID, &st.ct, &st.nonce, &st.dekID); err != nil {
			rows.Close()
			return 0, err
		}
		stales = append(stales, st)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for _, st := range stales {
		oldDEK, err := s.dekPlaintext(ctx, tx, st.dekID)
		if err != nil {
			return 0, err
		}
		aad := secretAAD(orgID, st.projectID, st.id)
		plain, err := gcmOpen(oldDEK, aad, st.nonce, st.ct)
		if err != nil {
			return 0, fmt.Errorf("re-encrypt: decrypt %s: %w", st.id, err)
		}
		newNonce, newCT, err := gcmSeal(activeDEK, aad, plain)
		if err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE secrets SET ciphertext = $2, nonce = $3, dek_id = $4, updated_at = now() WHERE id = $1`,
			st.id, newCT, newNonce, activeDEKID); err != nil {
			return 0, err
		}
	}

	// Retire any inactive DEK that no longer backs a secret.
	if _, err := tx.Exec(ctx, `
		UPDATE org_deks d SET retired_at = now()
		 WHERE d.org_id = $1 AND NOT d.active AND d.retired_at IS NULL
		   AND NOT EXISTS (SELECT 1 FROM secrets s WHERE s.dek_id = d.id)`, orgID); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(stales), nil
}
