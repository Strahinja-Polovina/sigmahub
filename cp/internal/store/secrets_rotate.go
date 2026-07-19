package store

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"

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
	// SIGMA-88: also re-wrap the org's DIRECTLY custody-wrapped envelopes (git
	// provider tokens, per-server bootstrap keys) so a transit-key rotation +
	// prune can't strand them. Each keeps its own purpose AAD.
	gitN, err := s.rewrapColumnTx(ctx, tx,
		`SELECT id, token_wrapped FROM git_connections WHERE org_id = $1 AND token_wrapped IS NOT NULL`,
		`UPDATE git_connections SET token_wrapped = $2 WHERE id = $1`,
		gitTokenPurpose(orgID), orgID)
	if err != nil {
		return 0, err
	}
	srvN, err := s.rewrapColumnTx(ctx, tx,
		`SELECT id, bootstrap_key_wrapped FROM servers WHERE org_id = $1 AND bootstrap_key_wrapped IS NOT NULL`,
		`UPDATE servers SET bootstrap_key_wrapped = $2 WHERE id = $1`,
		"srv_bootstrap:"+orgID, orgID)
	if err != nil {
		return 0, err
	}
	total := len(deks) + gitN + srvN
	if total > 0 {
		if err := auditTx(ctx, tx, orgID, actor, "KEK rotated",
			fmt.Sprintf("%d DEKs, %d git tokens, %d bootstrap keys re-wrapped", len(deks), gitN, srvN)); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return total, nil
}

// rewrapColumnTx re-wraps every non-null envelope yielded by selectSQL (which
// must return (id, wrapped) and take one string arg) under `purpose`, advancing
// it to the current custody key, then writes each back via updateSQL(id,
// wrapped). Rows are collected before any UPDATE so the read and writes don't
// contend on the same connection. Returns the count re-wrapped (SIGMA-88).
func (s *Store) rewrapColumnTx(ctx context.Context, tx pgx.Tx, selectSQL, updateSQL, purpose, arg string) (int, error) {
	rows, err := tx.Query(ctx, selectSQL, arg)
	if err != nil {
		return 0, err
	}
	type row struct {
		id      string
		wrapped []byte
	}
	var rs []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.wrapped); err != nil {
			rows.Close()
			return 0, err
		}
		rs = append(rs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, r := range rs {
		plain, err := s.custody.Unwrap(ctx, purpose, r.wrapped)
		if err != nil {
			return 0, fmt.Errorf("unwrap for rewrap (%s): %w", purpose, err)
		}
		w, err := s.custody.Wrap(ctx, purpose, plain)
		if err != nil {
			return 0, fmt.Errorf("rewrap (%s): %w", purpose, err)
		}
		if _, err := tx.Exec(ctx, updateSQL, r.id, w); err != nil {
			return 0, err
		}
	}
	return len(rs), nil
}

// RotateGlobalKEK re-wraps the CP-global custody-wrapped secrets (token pepper,
// DSD signing key, GitHub App key) under the current custody key, so a
// transit-key rotation + prune doesn't strand them. NOT org-scoped — call once,
// out of band of any tenant. Returns the number re-wrapped. Audited under "*".
// (SIGMA-88)
func (s *Store) RotateGlobalKEK(ctx context.Context, actor string) (int, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	n := 0
	for _, name := range []string{tokenPepperName, dsdSigningKeyName, githubAppKeyName} {
		var wrapped []byte
		err := tx.QueryRow(ctx, `SELECT wrapped FROM cp_secrets WHERE name = $1`, name).Scan(&wrapped)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return 0, err
		}
		plain, err := s.custody.Unwrap(ctx, name, wrapped)
		if err != nil {
			return 0, fmt.Errorf("unwrap %s: %w", name, err)
		}
		rew, err := s.custody.Wrap(ctx, name, plain)
		if err != nil {
			return 0, fmt.Errorf("rewrap %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `UPDATE cp_secrets SET wrapped = $2 WHERE name = $1`, name, rew); err != nil {
			return 0, err
		}
		n++
	}
	if n > 0 {
		if err := auditTx(ctx, tx, "*", actor, "Global KEK rotated", fmt.Sprintf("%d CP secrets re-wrapped", n)); err != nil {
			return 0, err
		}
	}
	return n, tx.Commit(ctx)
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

	// Re-encrypt EVERY table that stores ciphertext under the org DEK — not just
	// `secrets`. Missing a table here would (a) leave those credentials on the
	// old DEK (rotation gives them no protection) and (b) let the retirement
	// check below drop a DEK that still backs live ciphertexts — a latent
	// data-loss trap if a DEK is ever hard-deleted/revoked (SIGMA-68).
	total := 0
	for _, t := range dekReencTargets {
		n, err := s.reencTableTx(ctx, tx, orgID, activeDEKID, activeDEK, t)
		if err != nil {
			return 0, err
		}
		total += n
	}

	// Retire an inactive DEK ONLY when no table references it anymore — the
	// "retired ⇒ backs nothing" invariant must hold across every DEK-bearing
	// table, or a later hard-delete makes those ciphertexts undecryptable.
	if _, err := tx.Exec(ctx, `
		UPDATE org_deks d SET retired_at = now()
		 WHERE d.org_id = $1 AND NOT d.active AND d.retired_at IS NULL
		   AND NOT EXISTS (SELECT 1 FROM secrets         WHERE dek_id            = d.id)
		   AND NOT EXISTS (SELECT 1 FROM db_credentials  WHERE dek_id            = d.id)
		   AND NOT EXISTS (SELECT 1 FROM s3_credentials  WHERE dek_id            = d.id)
		   AND NOT EXISTS (SELECT 1 FROM backup_targets  WHERE dek_id            = d.id)
		   AND NOT EXISTS (SELECT 1 FROM backup_policies WHERE repo_dek_id       = d.id)
		   AND NOT EXISTS (SELECT 1 FROM alert_channels  WHERE dek_id            = d.id)
		   AND NOT EXISTS (SELECT 1 FROM s3_buckets      WHERE key_dek_id        = d.id)
		   AND NOT EXISTS (SELECT 1 FROM pending_s3_ops  WHERE new_secret_dek_id = d.id)`, orgID); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return total, nil
}

// dekReencTarget describes one DEK-bearing table for rotation re-encryption:
// its primary key, ciphertext/nonce/dek columns, the extra identity columns the
// AAD needs, and how to build that AAD (ids[0] is the pk value, ids[1:] the
// extra id columns in order).
type dekReencTarget struct {
	table, pkCol, ctCol, nonceCol, dekCol string
	idCols                                []string
	aad                                   func(orgID string, ids []string) []byte
}

// dekReencTargets is the exhaustive list of tables encrypted under the org DEK.
// Keep it in sync with every `REFERENCES org_deks(id)` column.
var dekReencTargets = []dekReencTarget{
	{"secrets", "id", "ciphertext", "nonce", "dek_id", []string{"project_id"},
		func(o string, id []string) []byte { return secretAAD(o, id[1], id[0]) }},
	{"db_credentials", "resource_id", "ciphertext", "nonce", "dek_id", nil,
		func(o string, id []string) []byte { return dbAAD(o, id[0]) }},
	{"s3_credentials", "resource_id", "ciphertext", "nonce", "dek_id", nil,
		func(o string, id []string) []byte { return s3AAD(o, id[0]) }},
	{"backup_targets", "id", "secret_ciphertext", "secret_nonce", "dek_id", nil,
		func(o string, id []string) []byte { return targetAAD(o, id[0]) }},
	{"backup_policies", "id", "repo_key_ciphertext", "repo_key_nonce", "repo_dek_id", nil,
		func(o string, id []string) []byte { return repoKeyAAD(o, id[0]) }},
	{"alert_channels", "id", "secret_ciphertext", "secret_nonce", "dek_id", nil,
		func(o string, id []string) []byte { return alertChannelAAD(o, id[0]) }},
	{"s3_buckets", "id", "key_ciphertext", "key_nonce", "key_dek_id", []string{"resource_id"},
		func(o string, id []string) []byte { return s3AAD(o, id[1]) }},
	{"pending_s3_ops", "id", "new_secret_ciphertext", "new_secret_nonce", "new_secret_dek_id", []string{"resource_id"},
		func(o string, id []string) []byte { return s3AAD(o, id[1]) }},
}

// reencTableTx re-encrypts every row of one DEK-bearing table not already on the
// active DEK: decrypt with the row's old DEK + its identity-bound AAD, re-seal
// under the active DEK with the SAME AAD, update in place. Column names come
// from the static target list (never user input). Returns rows rewritten.
func (s *Store) reencTableTx(ctx context.Context, tx pgx.Tx, orgID, activeDEKID string, activeDEK []byte, t dekReencTarget) (int, error) {
	cols := append([]string{t.pkCol}, t.idCols...)
	cols = append(cols, t.ctCol, t.nonceCol, t.dekCol)
	sel := fmt.Sprintf("SELECT %s FROM %s WHERE org_id = $1 AND %s IS NOT NULL AND %s <> $2",
		strings.Join(cols, ", "), t.table, t.dekCol, t.dekCol)
	rows, err := tx.Query(ctx, sel, orgID, activeDEKID)
	if err != nil {
		return 0, err
	}
	type rec struct {
		ids       []string
		ct, nonce []byte
		dekID     string
	}
	nIDs := 1 + len(t.idCols)
	var recs []rec
	for rows.Next() {
		ids := make([]string, nIDs)
		var ct, nonce []byte
		var dekID string
		dest := make([]any, 0, nIDs+3)
		for i := range ids {
			dest = append(dest, &ids[i])
		}
		dest = append(dest, &ct, &nonce, &dekID)
		if err := rows.Scan(dest...); err != nil {
			rows.Close()
			return 0, err
		}
		recs = append(recs, rec{ids: ids, ct: ct, nonce: nonce, dekID: dekID})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	upd := fmt.Sprintf("UPDATE %s SET %s = $1, %s = $2, %s = $3 WHERE %s = $4",
		t.table, t.ctCol, t.nonceCol, t.dekCol, t.pkCol)
	for _, r := range recs {
		oldDEK, err := s.dekPlaintext(ctx, tx, r.dekID)
		if err != nil {
			return 0, err
		}
		aad := t.aad(orgID, r.ids)
		plain, err := gcmOpen(oldDEK, aad, r.nonce, r.ct)
		if err != nil {
			return 0, fmt.Errorf("re-encrypt %s %s: %w", t.table, r.ids[0], err)
		}
		newNonce, newCT, err := gcmSeal(activeDEK, aad, plain)
		if err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, upd, newCT, newNonce, activeDEKID, r.ids[0]); err != nil {
			return 0, err
		}
	}
	return len(recs), nil
}
