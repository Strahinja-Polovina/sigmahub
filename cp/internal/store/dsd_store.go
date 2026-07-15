package store

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/kms"
)

const dsdSigningKeyName = "dsd_signing_key"

// LoadDSDSigningKey returns the CP's Ed25519 DSD-signing key, generating and
// wrapping the seed on first boot (same custody path as the token pepper).
// The key is stable across restarts because agents pin its public half at
// enrollment; unwrapping it emits one audit event via the custody sink.
func (s *Store) LoadDSDSigningKey(ctx context.Context, custody kms.KeyCustody) (ed25519.PrivateKey, error) {
	var wrapped []byte
	err := s.Pool.QueryRow(ctx,
		`SELECT wrapped FROM cp_secrets WHERE name = $1`, dsdSigningKeyName).Scan(&wrapped)

	if errors.Is(err, pgx.ErrNoRows) {
		_, priv, genErr := ed25519.GenerateKey(nil)
		if genErr != nil {
			return nil, genErr
		}
		seed := priv.Seed()
		wrapped, err = custody.Wrap(ctx, dsdSigningKeyName, seed)
		if err != nil {
			return nil, fmt.Errorf("wrap dsd key: %w", err)
		}
		if _, err := s.Pool.Exec(ctx, `
			INSERT INTO cp_secrets (name, wrapped) VALUES ($1, $2)
			ON CONFLICT (name) DO NOTHING`, dsdSigningKeyName, wrapped); err != nil {
			return nil, fmt.Errorf("insert dsd key: %w", err)
		}
		if err := s.Pool.QueryRow(ctx,
			`SELECT wrapped FROM cp_secrets WHERE name = $1`, dsdSigningKeyName).Scan(&wrapped); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}

	seed, err := custody.Unwrap(ctx, dsdSigningKeyName, wrapped)
	if err != nil {
		return nil, fmt.Errorf("unwrap dsd key: %w", err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("dsd key seed is %d bytes, want %d", len(seed), ed25519.SeedSize)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// GetDSD returns the current signed DSD for a server, or ErrNotFound when none
// has been rendered yet (version 0).
func (s *Store) GetDSD(ctx context.Context, serverID string) (dsd.Signed, error) {
	var docJSON []byte
	var sig []byte
	err := s.Pool.QueryRow(ctx, `
		SELECT doc, signature FROM server_dsd WHERE server_id = $1 AND version > 0`,
		serverID).Scan(&docJSON, &sig)
	if errors.Is(err, pgx.ErrNoRows) {
		return dsd.Signed{}, ErrNotFound
	}
	if err != nil {
		return dsd.Signed{}, err
	}
	var doc dsd.Document
	if err := json.Unmarshal(docJSON, &doc); err != nil {
		return dsd.Signed{}, err
	}
	return dsd.Signed{Document: doc, Signature: sig}, nil
}

// CurrentDSDVersion returns the server's current DSD version (0 if none).
func (s *Store) CurrentDSDVersion(ctx context.Context, serverID string) (int64, error) {
	var v int64
	err := s.Pool.QueryRow(ctx,
		`SELECT version FROM server_dsd WHERE server_id = $1`, serverID).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return v, err
}

// StoreDSD persists a newly rendered DSD only if docHash changed, bumping the
// version monotonically and signing under key. Returns the stored signed
// document and whether it changed (false → nothing to deliver). One audit row
// is written per issued (changed) DSD. The row is created lazily on first
// render (INSERT ... ON CONFLICT), so the reconciler needs no pre-seeding.
func (s *Store) StoreDSD(ctx context.Context, orgID, serverID string, ops []dsd.Op, docHash string, priv ed25519.PrivateKey) (dsd.Signed, bool, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return dsd.Signed{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock the row so concurrent reconciles (event + resync) serialize and the
	// version never regresses or duplicates.
	var curVersion int64
	var curHash string
	err = tx.QueryRow(ctx, `
		INSERT INTO server_dsd (server_id, org_id, version, doc_hash)
		VALUES ($1, $2, 0, '')
		ON CONFLICT (server_id) DO UPDATE SET server_id = server_dsd.server_id
		RETURNING version, doc_hash`,
		serverID, orgID).Scan(&curVersion, &curHash)
	if err != nil {
		return dsd.Signed{}, false, fmt.Errorf("lock dsd row: %w", err)
	}
	if curHash == docHash && curVersion > 0 {
		return dsd.Signed{}, false, tx.Commit(ctx)
	}

	next := curVersion + 1
	doc := dsd.Document{Version: next, OrgID: orgID, ServerID: serverID, Ops: ops}
	sig, err := dsd.Sign(priv, doc)
	if err != nil {
		return dsd.Signed{}, false, err
	}
	docJSON, err := json.Marshal(doc)
	if err != nil {
		return dsd.Signed{}, false, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE server_dsd
		   SET version = $2, doc = $3, signature = $4, doc_hash = $5, updated_at = now()
		 WHERE server_id = $1`,
		serverID, next, docJSON, sig, docHash); err != nil {
		return dsd.Signed{}, false, fmt.Errorf("update dsd: %w", err)
	}
	if err := auditTx(ctx, tx, orgID, "reconciler", "DSD issued",
		fmt.Sprintf("%s v%d", serverID, next)); err != nil {
		return dsd.Signed{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return dsd.Signed{}, false, err
	}
	return dsd.Signed{Document: doc, Signature: sig}, true, nil
}

// ResourceSpecsForServer returns the rows the reconciler renders a server's DSD
// from. ProjectID drives per-project Docker network naming; Ephemeral flags
// preview resources whose teardown skips interactive approval.
type ResourceSpec struct {
	ResourceID string
	ProjectID  string
	Kind       string
	Spec       json.RawMessage
	Ephemeral  bool
}

func (s *Store) ResourceSpecsForServer(ctx context.Context, serverID string) ([]ResourceSpec, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, project_id, kind, spec, ephemeral FROM resources WHERE server_id = $1 ORDER BY created_at`,
		serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ResourceSpec{}
	for rows.Next() {
		var r ResourceSpec
		if err := rows.Scan(&r.ResourceID, &r.ProjectID, &r.Kind, &r.Spec, &r.Ephemeral); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PendingDestructiveOp is a confirmed destructive action awaiting agent
// application, rendered into the server's DSD until applied.
type PendingDestructiveOp struct {
	ID      string
	OpKind  string
	Target  string
}

// PendingDestructiveOpsForServer returns a server's still-unapplied destructive
// ops (applied_at IS NULL), oldest first for deterministic op ordering.
func (s *Store) PendingDestructiveOpsForServer(ctx context.Context, serverID string) ([]PendingDestructiveOp, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, op_kind, target FROM pending_destructive_ops
		WHERE server_id = $1 AND applied_at IS NULL ORDER BY created_at, id`,
		serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PendingDestructiveOp{}
	for rows.Next() {
		var p PendingDestructiveOp
		if err := rows.Scan(&p.ID, &p.OpKind, &p.Target); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// MarkDestructiveOpApplied records that the agent applied a destructive op so it
// drops out of future DSDs.
func (s *Store) MarkDestructiveOpApplied(ctx context.Context, id string) error {
	_, err := s.Pool.Exec(ctx,
		`UPDATE pending_destructive_ops SET applied_at = now() WHERE id = $1 AND applied_at IS NULL`, id)
	return err
}

// AllServerIDs lists every non-deleted server id, for the periodic resync.
func (s *Store) AllServerIDs(ctx context.Context) ([]struct{ ServerID, OrgID string }, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, org_id FROM servers ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []struct{ ServerID, OrgID string }
	for rows.Next() {
		var id, org string
		if err := rows.Scan(&id, &org); err != nil {
			return nil, err
		}
		out = append(out, struct{ ServerID, OrgID string }{id, org})
	}
	return out, rows.Err()
}

// ApplyDSDStatus records agent-reported op results for a server at a given DSD
// version. Status for a version below the last recorded one is ignored
// (superseded). Per-resource op states are written into resources.status.
// Returns false when the report was superseded (ignored).
func (s *Store) ApplyDSDStatus(ctx context.Context, serverID string, version int64, opStatus map[string]json.RawMessage) (bool, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var applied, issued int64
	err = tx.QueryRow(ctx,
		`SELECT applied_version, version FROM server_dsd WHERE server_id = $1 FOR UPDATE`, serverID).Scan(&applied, &issued)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	// Reject superseded (below last-applied) and bogus (above the version the
	// CP has actually issued) reports. The reported version is agent-supplied,
	// so without the upper bound a compromised/buggy agent could set
	// applied_version arbitrarily high and permanently freeze this server's
	// status — every honest report would then read as superseded.
	if version < applied || version > issued {
		return false, tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE server_dsd SET applied_version = $2 WHERE server_id = $1`, serverID, version); err != nil {
		return false, err
	}
	for resourceID, st := range opStatus {
		// Only touch resources that belong to this server (defence in depth
		// against a compromised agent reporting foreign resource ids).
		if _, err := tx.Exec(ctx,
			`UPDATE resources SET status = $3, updated_at = now() WHERE id = $1 AND server_id = $2`,
			resourceID, serverID, st); err != nil {
			return false, fmt.Errorf("update resource status: %w", err)
		}
	}
	return true, tx.Commit(ctx)
}
