package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Secret is a tenant secret's metadata. The plaintext value is never carried on
// this type — it is only ever returned by an explicit, audited reveal/resolve.
type Secret struct {
	ID            string  `json:"id"`
	ProjectID     string  `json:"projectId"`
	EnvironmentID *string `json:"environmentId"`
	Name          string  `json:"name"`
	EnvVar        bool    `json:"envVar"`
	CreatedBy     string  `json:"createdBy"`
}

// CreateSecretInput describes a new secret. EnvironmentID "" means project-scoped.
type CreateSecretInput struct {
	ProjectID     string
	EnvironmentID string
	Name          string
	Value         string
	EnvVar        bool
}

// ResolvedSecret is a decrypted secret handed to an agent for injection.
type ResolvedSecret struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	EnvVar bool   `json:"envVar"`
}

// secretAAD binds a ciphertext to its row identity. GCM verifies it on decrypt,
// so a ciphertext copied into another org's/secret's row fails to open.
func secretAAD(orgID, projectID, secretID string) []byte {
	return []byte(orgID + "|" + projectID + "|" + secretID)
}

func gcmSeal(dek, aad, plaintext []byte) (nonce, ciphertext []byte, err error) {
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	return nonce, aead.Seal(nil, nonce, plaintext, aad), nil
}

func gcmOpen(dek, aad, nonce, ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return aead.Open(nil, nonce, ciphertext, aad)
}

func (s *Store) cacheDEK(dekID string, dek []byte) {
	s.dekMu.Lock()
	s.dekCache[dekID] = dek
	s.dekMu.Unlock()
}

// dekPlaintext returns the (cached) unwrapped DEK for a dek id. The custody
// Unwrap is audited, so a DEK is unwrapped once per process, not per secret op.
func (s *Store) dekPlaintext(ctx context.Context, q pgxQuerier, dekID string) ([]byte, error) {
	s.dekMu.Lock()
	if d, ok := s.dekCache[dekID]; ok {
		s.dekMu.Unlock()
		return d, nil
	}
	s.dekMu.Unlock()

	var orgID string
	var wrapped []byte
	if err := q.QueryRow(ctx, `SELECT org_id, wrapped_dek FROM org_deks WHERE id = $1`, dekID).Scan(&orgID, &wrapped); err != nil {
		return nil, err
	}
	dek, err := s.custody.Unwrap(ctx, "org_dek:"+orgID, wrapped)
	if err != nil {
		return nil, fmt.Errorf("unwrap dek: %w", err)
	}
	if len(dek) != 32 {
		return nil, fmt.Errorf("dek: wrong key size %d", len(dek))
	}
	s.cacheDEK(dekID, dek)
	return dek, nil
}

// pgxQuerier is satisfied by both *pgxpool.Pool and pgx.Tx.
type pgxQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// activeDEKTx returns the org's active DEK id + plaintext, generating and
// wrapping a fresh one on first use. Runs inside the caller's transaction.
func (s *Store) activeDEKTx(ctx context.Context, tx pgx.Tx, orgID string) (string, []byte, error) {
	var dekID string
	err := tx.QueryRow(ctx,
		`SELECT id FROM org_deks WHERE org_id = $1 AND active LIMIT 1`, orgID).Scan(&dekID)
	if errors.Is(err, pgx.ErrNoRows) {
		dek := make([]byte, 32)
		if _, err := rand.Read(dek); err != nil {
			return "", nil, err
		}
		wrapped, err := s.custody.Wrap(ctx, "org_dek:"+orgID, dek)
		if err != nil {
			return "", nil, fmt.Errorf("wrap dek: %w", err)
		}
		dekID = newID("dek")
		if _, err := tx.Exec(ctx,
			`INSERT INTO org_deks (id, org_id, wrapped_dek) VALUES ($1, $2, $3)`, dekID, orgID, wrapped); err != nil {
			return "", nil, fmt.Errorf("insert dek: %w", err)
		}
		s.cacheDEK(dekID, dek)
		return dekID, dek, nil
	}
	if err != nil {
		return "", nil, err
	}
	dek, err := s.dekPlaintext(ctx, tx, dekID)
	return dekID, dek, err
}

// assertScope confirms the project belongs to the org and (if set) the
// environment belongs to the project — tenant isolation for secret writes.
func assertScopeTx(ctx context.Context, tx pgx.Tx, orgID, projectID, envID string) error {
	var one int
	err := tx.QueryRow(ctx, `SELECT 1 FROM projects WHERE id = $1 AND org_id = $2`, projectID, orgID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if envID != "" {
		err := tx.QueryRow(ctx, `SELECT 1 FROM environments WHERE id = $1 AND project_id = $2`, envID, projectID).Scan(&one)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalid{Msg: "environment does not belong to the project"}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// CreateSecret encrypts and stores a secret under the org's active DEK, bound by
// AAD to its row identity. Audited.
func (s *Store) CreateSecret(ctx context.Context, orgID, actor string, in CreateSecretInput) (Secret, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Secret{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := assertScopeTx(ctx, tx, orgID, in.ProjectID, in.EnvironmentID); err != nil {
		return Secret{}, err
	}
	dekID, dek, err := s.activeDEKTx(ctx, tx, orgID)
	if err != nil {
		return Secret{}, err
	}
	secretID := newID("sec")
	nonce, ct, err := gcmSeal(dek, secretAAD(orgID, in.ProjectID, secretID), []byte(in.Value))
	if err != nil {
		return Secret{}, err
	}
	var envID *string
	if in.EnvironmentID != "" {
		envID = &in.EnvironmentID
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO secrets (id, org_id, project_id, environment_id, name, ciphertext, nonce, dek_id, env_var, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		secretID, orgID, in.ProjectID, envID, in.Name, ct, nonce, dekID, in.EnvVar, actor)
	if isUniqueViolation(err) {
		return Secret{}, fmt.Errorf("%w: a secret named %q already exists in this scope", ErrConflict, in.Name)
	}
	if err != nil {
		return Secret{}, fmt.Errorf("insert secret: %w", err)
	}
	if err := auditTx(ctx, tx, orgID, actor, "Secret created", in.Name); err != nil {
		return Secret{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Secret{}, err
	}
	return Secret{ID: secretID, ProjectID: in.ProjectID, EnvironmentID: envID, Name: in.Name, EnvVar: in.EnvVar, CreatedBy: actor}, nil
}

// ListSecrets returns secret METADATA (never values) for a project, optionally
// filtered to one environment (envID "") means project-scoped only.
func (s *Store) ListSecrets(ctx context.Context, orgID, projectID, envID string) ([]Secret, error) {
	q := `SELECT id, project_id, environment_id, name, env_var, created_by
	        FROM secrets WHERE org_id = $1 AND project_id = $2`
	args := []any{orgID, projectID}
	if envID != "" {
		q += ` AND environment_id = $3`
		args = append(args, envID)
	}
	q += ` ORDER BY name`
	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Secret{}
	for rows.Next() {
		var sec Secret
		if err := rows.Scan(&sec.ID, &sec.ProjectID, &sec.EnvironmentID, &sec.Name, &sec.EnvVar, &sec.CreatedBy); err != nil {
			return nil, err
		}
		out = append(out, sec)
	}
	return out, rows.Err()
}

// RevealSecret decrypts and returns a secret's value, writing an audit row (the
// "audit on EVERY read" invariant). The API gates this at Project Admin+.
func (s *Store) RevealSecret(ctx context.Context, orgID, secretID, actor string) (string, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var projectID, name, dekID string
	var ct, nonce []byte
	err = tx.QueryRow(ctx, `
		SELECT project_id, name, ciphertext, nonce, dek_id
		  FROM secrets WHERE org_id = $1 AND id = $2`, orgID, secretID).Scan(&projectID, &name, &ct, &nonce, &dekID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	dek, err := s.dekPlaintext(ctx, tx, dekID)
	if err != nil {
		return "", err
	}
	plaintext, err := gcmOpen(dek, secretAAD(orgID, projectID, secretID), nonce, ct)
	if err != nil {
		return "", fmt.Errorf("decrypt secret: %w", err)
	}
	if err := auditTx(ctx, tx, orgID, actor, "Secret revealed", name); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return string(plaintext), nil
}

// DeleteSecret removes a secret. Audited.
func (s *Store) DeleteSecret(ctx context.Context, orgID, secretID, actor string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var name string
	err = tx.QueryRow(ctx,
		`DELETE FROM secrets WHERE org_id = $1 AND id = $2 RETURNING name`, orgID, secretID).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := auditTx(ctx, tx, orgID, actor, "Secret deleted", name); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
