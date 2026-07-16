package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dbengine"
)

// DBCredentials is a database resource's generated credential set with the
// password DECRYPTED — only ever returned through audited, RBAC-gated paths
// (the Project Admin reveal and the agent's server-scoped secret fetch).
type DBCredentials struct {
	Engine   string `json:"engine"`
	Username string `json:"username"`
	Database string `json:"database"`
	Password string `json:"password"`
}

// dbCredAAD binds a password ciphertext to its row identity (same pattern as
// secretAAD); GCM authenticates it on open, so a ciphertext moved to another
// row/org fails to decrypt.
func dbCredAAD(orgID, resourceID, credID string) []byte {
	return []byte("dbcred\x00" + orgID + "\x00" + resourceID + "\x00" + credID)
}

// randomToken returns a hex string of n random bytes — the generated password
// alphabet is conservative so every engine's URL/conf syntax accepts it verbatim.
func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// provisionDatabaseTx generates credentials for a new database resource and
// writes its default backup-policy row, inside the resource-creation tx. The
// schedule default keys off the environment's production flag: daily at 03:00
// for production, weekly (Sunday 04:00) otherwise. P1-11 owns execution.
func (s *Store) provisionDatabaseTx(ctx context.Context, tx pgx.Tx, orgID, resourceID, kind, envID string) error {
	if _, ok := dbengine.Get(kind); !ok {
		return nil // not a database kind — nothing to provision
	}
	password, err := randomToken(24)
	if err != nil {
		return err
	}
	credID := newID("dbc")
	dekID, dek, err := s.activeDEKTx(ctx, tx, orgID)
	if err != nil {
		return err
	}
	nonce, ct, err := gcmSeal(dek, dbCredAAD(orgID, resourceID, credID), []byte(password))
	if err != nil {
		return err
	}
	// Deterministic, engine-safe identifiers shared with the reconciler's render
	// (which must emit the same username/db in container env without a query).
	username, dbName := dbengine.DerivedIdentity(resourceID)
	if _, err := tx.Exec(ctx, `
		INSERT INTO db_credentials (id, org_id, resource_id, engine, username, db_name, ciphertext, nonce, dek_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		credID, orgID, resourceID, kind, username, dbName, ct, nonce, dekID); err != nil {
		return err
	}

	// Backup-policy hook (P1-11 executes): production environments default to a
	// daily backup, everything else weekly.
	production := false
	if envID != "" {
		if err := tx.QueryRow(ctx,
			`SELECT production FROM environments WHERE org_id = $1 AND id = $2`, orgID, envID).Scan(&production); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	schedule := "0 4 * * 0" // weekly, Sunday 04:00
	retention := 7
	if production {
		schedule = "0 3 * * *" // daily 03:00
		retention = 14
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO backup_policies (id, org_id, resource_id, schedule, retention_days)
		VALUES ($1,$2,$3,$4,$5)`,
		newID("bkp"), orgID, resourceID, schedule, retention)
	return err
}

// DBCredentialsForResource decrypts a database resource's credentials. NOT
// audited/gated here — callers own that: the reveal API enforces Project Admin+
// and writes the audit row; the agent path is server-scoped.
func (s *Store) DBCredentialsForResource(ctx context.Context, orgID, resourceID string) (DBCredentials, error) {
	var (
		out       DBCredentials
		credID    string
		dekID     string
		ct, nonce []byte
	)
	err := s.Pool.QueryRow(ctx, `
		SELECT id, engine, username, db_name, ciphertext, nonce, dek_id
		  FROM db_credentials WHERE org_id = $1 AND resource_id = $2`,
		orgID, resourceID).Scan(&credID, &out.Engine, &out.Username, &out.Database, &ct, &nonce, &dekID)
	if errors.Is(err, pgx.ErrNoRows) {
		return DBCredentials{}, ErrNotFound
	}
	if err != nil {
		return DBCredentials{}, err
	}
	dek, err := s.dekPlaintext(ctx, s.Pool, dekID)
	if err != nil {
		return DBCredentials{}, err
	}
	plaintext, err := gcmOpen(dek, dbCredAAD(orgID, resourceID, credID), nonce, ct)
	if err != nil {
		return DBCredentials{}, fmt.Errorf("decrypt db credential: %w", err)
	}
	out.Password = string(plaintext)
	return out, nil
}

// RevealDBConnection returns the resource's connection string (mesh-internal
// host), auditing the reveal. RBAC (Project Admin+) is enforced at the API
// layer; every call that returns a credential writes an audit row.
func (s *Store) RevealDBConnection(ctx context.Context, orgID, resourceID, actor string) (string, error) {
	creds, err := s.DBCredentialsForResource(ctx, orgID, resourceID)
	if err != nil {
		return "", err
	}
	eng, ok := dbengine.Get(creds.Engine)
	if !ok {
		return "", ErrNotFound
	}
	// The connection host is the hosting server's mesh address — reachable only
	// across the org's WireGuard mesh (P1-4), never from the public internet.
	var meshIP *string
	err = s.Pool.QueryRow(ctx, `
		SELECT s.mesh_ip FROM resources r JOIN servers s ON s.id = r.server_id
		 WHERE r.org_id = $1 AND r.id = $2`, orgID, resourceID).Scan(&meshIP)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	host := "<mesh-ip-pending>"
	if meshIP != nil && *meshIP != "" {
		host = *meshIP
	}
	conn := eng.ConnString(dbengine.Credentials{Username: creds.Username, Password: creds.Password, Database: creds.Database}, host, eng.Port)
	if _, err := s.Pool.Exec(ctx, `
		INSERT INTO cp_audit_log (org_id, actor, action, target)
		VALUES ($1, $2, 'Database connection revealed', $3)`, orgID, actor, resourceID); err != nil {
		return "", err
	}
	return conn, nil
}

// BackupPolicy is the P1-11 hook row created with every database resource.
type BackupPolicy struct {
	ID            string `json:"id"`
	ResourceID    string `json:"resourceId"`
	Schedule      string `json:"schedule"`
	RetentionDays int    `json:"retentionDays"`
	Enabled       bool   `json:"enabled"`
}

// BackupPolicyForResource returns a database resource's backup policy.
func (s *Store) BackupPolicyForResource(ctx context.Context, orgID, resourceID string) (BackupPolicy, error) {
	var p BackupPolicy
	err := s.Pool.QueryRow(ctx, `
		SELECT id, resource_id, schedule, retention_days, enabled
		  FROM backup_policies WHERE org_id = $1 AND resource_id = $2`,
		orgID, resourceID).Scan(&p.ID, &p.ResourceID, &p.Schedule, &p.RetentionDays, &p.Enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return BackupPolicy{}, ErrNotFound
	}
	return p, err
}

// dbSecretsForResource resolves the engine-specific secret injections for a
// database resource (the password as an env var or a seeded conf file),
// appended to the agent's secret fetch. Returns nil for non-DB resources.
func (s *Store) dbSecretsForResource(ctx context.Context, orgID, resourceID string) ([]ResolvedSecret, error) {
	creds, err := s.DBCredentialsForResource(ctx, orgID, resourceID)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	eng, ok := dbengine.Get(creds.Engine)
	if !ok {
		return nil, nil
	}
	if eng.Secret.EnvName != "" {
		return []ResolvedSecret{{Name: eng.Secret.EnvName, Value: creds.Password, EnvVar: true}}, nil
	}
	if eng.Secret.FileName != "" && eng.Secret.FileContent != nil {
		return []ResolvedSecret{{Name: eng.Secret.FileName, Value: eng.Secret.FileContent(creds.Password), EnvVar: false}}, nil
	}
	return nil, nil
}
