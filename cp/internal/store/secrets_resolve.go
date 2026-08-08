package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// SecretRefMeta is a secret's non-secret metadata for DSD rendering: the name
// and injection mode, never the value. The reconciler emits these as references
// so a captured DSD leaks nothing.
type SecretRefMeta struct {
	ResourceID string
	Name       string
	EnvVar     bool
}

// SecretRefsForServer returns the effective secret references (metadata only,
// no values) for every resource on a server, keyed by resource id. Environment
// secrets override project-scoped defaults of the same name. Used by the
// reconciler to render container ops that carry references; no decryption or
// audit happens here (nothing is revealed).
func (s *Store) SecretRefsForServer(ctx context.Context, serverID string) (map[string][]SecretRefMeta, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT r.id, s.name, s.env_var
		  FROM resources r
		  JOIN secrets s
		    ON s.org_id = r.org_id AND s.project_id = r.project_id
		   AND (s.environment_id = r.environment_id OR s.environment_id IS NULL)
		 WHERE`+ResourceHostedHereClause+`
		 ORDER BY r.id, (s.environment_id IS NULL), s.name`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]SecretRefMeta{}
	seen := map[string]bool{} // resourceID+"\x00"+name
	for rows.Next() {
		var m SecretRefMeta
		if err := rows.Scan(&m.ResourceID, &m.Name, &m.EnvVar); err != nil {
			return nil, err
		}
		key := m.ResourceID + "\x00" + m.Name
		if seen[key] {
			continue // env secret already provided this name; skip project default
		}
		seen[key] = true
		out[m.ResourceID] = append(out[m.ResourceID], m)
	}
	return out, rows.Err()
}

// ResolveSecretsForResource decrypts the secrets a resource should receive:
// its environment's secrets, plus project-scoped secrets not overridden by an
// environment secret of the same name (env overrides project). Every fetch is
// audited (the "audit on every read incl. agent fetch" invariant).
//
// The resource must be scheduled onto the REQUESTING server (server_id), not
// merely belong to the org: an agent token authenticates one specific server, so
// scoping by org alone would let any compromised/stolen agent token drain every
// resource's decrypted secrets across the whole org (BOLA). This mirrors
// SecretRefsForServer's WHERE r.server_id constraint, so the value-fetch path
// grants exactly what the reference/DSD path already delivered to this host.
func (s *Store) ResolveSecretsForResource(ctx context.Context, orgID, serverID, resourceID, actor string) ([]ResolvedSecret, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var projectID string
	var envID *string
	err = tx.QueryRow(ctx,
		`SELECT project_id, environment_id FROM resources WHERE org_id = $1 AND id = $2 AND server_id = $3`,
		orgID, resourceID, serverID).Scan(&projectID, &envID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	// Environment secrets win over project-scoped defaults of the same name.
	// The ORDER BY puts env rows (environment_id NOT NULL → FALSE) first, so the
	// first occurrence of each name is the effective one.
	rows, err := tx.Query(ctx, `
		SELECT id, name, ciphertext, nonce, dek_id, env_var
		  FROM secrets
		 WHERE org_id = $1 AND project_id = $2
		   AND (environment_id = $3 OR environment_id IS NULL)
		 ORDER BY (environment_id IS NULL), name`,
		orgID, projectID, envID)
	if err != nil {
		return nil, err
	}
	type encRow struct {
		id, name, dekID string
		ct, nonce       []byte
		envVar          bool
	}
	var encRows []encRow
	seen := map[string]bool{}
	for rows.Next() {
		var r encRow
		if err := rows.Scan(&r.id, &r.name, &r.ct, &r.nonce, &r.dekID, &r.envVar); err != nil {
			rows.Close()
			return nil, err
		}
		if seen[r.name] {
			continue // project default overridden by an env secret of the same name
		}
		seen[r.name] = true
		encRows = append(encRows, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]ResolvedSecret, 0, len(encRows))
	for _, r := range encRows {
		dek, err := s.dekPlaintext(ctx, tx, r.dekID)
		if err != nil {
			return nil, err
		}
		plaintext, err := gcmOpen(dek, secretAAD(orgID, projectID, r.id), r.nonce, r.ct)
		if err != nil {
			return nil, fmt.Errorf("decrypt secret %q: %w", r.name, err)
		}
		out = append(out, ResolvedSecret{Name: r.name, Value: string(plaintext), EnvVar: r.envVar})
	}

	// P1-10: a database resource's generated credentials ride the same audited
	// resolve channel, appended LAST so the engine env names always win over a
	// same-named user secret in the agent's by-name merge.
	dbSecrets, err := s.resolveDBSecretsTx(ctx, tx, orgID, serverID, resourceID)
	if err != nil {
		return nil, err
	}
	out = append(out, dbSecrets...)
	// P2-1: S3 root credentials ride the same channel under the same rule.
	s3Secrets, err := s.resolveS3SecretsTx(ctx, tx, orgID, serverID, resourceID)
	if err != nil {
		return nil, err
	}
	out = append(out, s3Secrets...)

	if len(out) > 0 {
		if err := auditTx(ctx, tx, orgID, actor, "Secrets fetched (agent)", resourceID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}
