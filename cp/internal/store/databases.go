package store

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// dbPortBase is the first mesh-bound host port the allocator hands out. The
// range is per-server (unique index backstop) and never collides with the
// engines' well-known container ports.
const dbPortBase = 15000

// ErrNotDatabase marks a database-only endpoint called on a non-database
// resource; the API maps it to 404.
var ErrNotDatabase = errors.New("resource is not a database")

// dbCredentialsJSON is the envelope-encrypted payload of a db_credentials row.
// RootPassword is set only for engines with a separate superuser (MySQL).
type dbCredentialsJSON struct {
	Password     string `json:"password"`
	RootPassword string `json:"rootPassword,omitempty"`
}

// dbAAD binds a credentials ciphertext to its row identity, mirroring
// secretAAD: a ciphertext copied onto another resource's row fails to open.
func dbAAD(orgID, resourceID string) []byte {
	return []byte(orgID + "|db|" + resourceID)
}

// SetEnabledDBEngines installs the engine allowlist (CP_DB_ENGINES). The
// pre-agreed M6 fallback — cut to Postgres-only — must be a configuration cut,
// not a rewrite: disabling an engine only gates NEW resource creation here.
func (s *Store) SetEnabledDBEngines(engines []string) {
	m := map[string]bool{}
	for _, e := range engines {
		m[strings.TrimSpace(e)] = true
	}
	s.enabledDBEngines = m
}

// dbEngineEnabled reports whether new resources of this engine may be created.
// A nil allowlist (SetEnabledDBEngines never called) enables every engine.
func (s *Store) dbEngineEnabled(kind string) bool {
	if s.enabledDBEngines == nil {
		return true
	}
	return s.enabledDBEngines[kind]
}

// randomDBSecret returns a URL- and shell-safe 32-char credential.
func randomDBSecret() string {
	return hex.EncodeToString(randBytes(16))
}

// dbSafeName derives an identifier-safe database name from a resource name:
// lowercased, non-alphanumerics collapsed to underscores, leading digit
// prefixed. Falls back to "app".
func dbSafeName(name string) string {
	var b strings.Builder
	lastUnderscore := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			lastUnderscore = false
		default:
			if !lastUnderscore && b.Len() > 0 {
				b.WriteByte('_')
				lastUnderscore = true
			}
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "app"
	}
	if out[0] >= '0' && out[0] <= '9' {
		out = "db_" + out
	}
	if len(out) > 48 {
		out = out[:48]
	}
	return out
}

// allocateDBPort hands out the next free mesh-bound host port on a server.
// Callers run it inside the provisioning transaction; the per-server advisory
// lock serializes concurrent creates so MAX+1 can't collide (the unique index
// on (server_id, port) is the backstop).
func allocateDBPort(ctx context.Context, tx pgx.Tx, serverID string) (int, error) {
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext('dbport:' || $1))`, serverID); err != nil {
		return 0, fmt.Errorf("db port lock: %w", err)
	}
	// GREATEST over BOTH port-owning tables, symmetric with allocateS3Port: the
	// availability matrix keeps S3 and DB engines off the same server today, but
	// scanning only db_credentials would collide the moment that ever widens
	// (SIGMA-81). The shared advisory lock + (server_id, port) unique index back
	// this up.
	var port int
	err := tx.QueryRow(ctx, `
		SELECT GREATEST(
			COALESCE((SELECT MAX(port) FROM db_credentials WHERE server_id = $1), $2 - 1),
			COALESCE((SELECT MAX(port) FROM s3_credentials WHERE server_id = $1), $2 - 1)
		) + 1`, serverID, dbPortBase).Scan(&port)
	if err != nil {
		return 0, fmt.Errorf("db port max: %w", err)
	}
	return port, nil
}

// provisionDatabaseTx generates and stores a new database resource's
// credentials (envelope-encrypted under the org DEK), allocates its mesh-bound
// port and writes the default backup-policy row (the P1-11 hook). Runs inside
// CreateResource's transaction so a failed step never leaves a half-provisioned
// database. Returns the allocated port for the audit trail.
func (s *Store) provisionDatabaseTx(ctx context.Context, tx pgx.Tx, orgID string, r Resource, production bool) error {
	def, ok := DBEngine(r.Kind)
	if !ok {
		return fmt.Errorf("provision database: unknown engine %q", r.Kind)
	}
	port, err := allocateDBPort(ctx, tx, r.ServerID)
	if err != nil {
		return err
	}
	creds := dbCredentialsJSON{Password: randomDBSecret()}
	if def.Engine == "mysql" {
		creds.RootPassword = randomDBSecret()
	}
	plaintext, err := json.Marshal(creds)
	if err != nil {
		return err
	}
	dekID, dek, err := s.activeDEKTx(ctx, tx, orgID)
	if err != nil {
		return err
	}
	nonce, ct, err := gcmSeal(dek, dbAAD(orgID, r.ID), plaintext)
	if err != nil {
		return err
	}
	username := "sigma"
	dbname := dbSafeName(r.Name)
	if _, err := tx.Exec(ctx, `
		INSERT INTO db_credentials (resource_id, org_id, server_id, engine, username, dbname, port, ciphertext, nonce, dek_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		r.ID, orgID, r.ServerID, def.Engine, username, dbname, port, ct, nonce, dekID); err != nil {
		return fmt.Errorf("insert db credentials: %w", err)
	}
	// Default backup policy (P1-11 hook): production keeps 30 dailies, the rest
	// the GFS 7/4/6 default. Execution belongs entirely to P1-11.
	keepDaily, keepWeekly, keepMonthly := 7, 4, 6
	if production {
		keepDaily, keepWeekly, keepMonthly = 30, 0, 0
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO backup_policies (id, org_id, resource_id, schedule, keep_daily, keep_weekly, keep_monthly)
		VALUES ($1, $2, $3, 'daily', $4, $5, $6)`,
		newID("bkp"), orgID, r.ID, keepDaily, keepWeekly, keepMonthly); err != nil {
		return fmt.Errorf("insert backup policy: %w", err)
	}
	return nil
}

// DBTarget is the reconciler's render input for one database resource: engine
// identity plus the generated non-secret credential parts and the mesh port.
// The password never rides here — the DSD carries secret references only.
type DBTarget struct {
	Engine     string
	Username   string
	Database   string
	Port       int
	ServerType string // drives the tuning profile (database vs general)
	// PITR (P2-5): render WAL archiving (spool volume + archive flags).
	PITR bool
}

// DBTargetsForServer returns the database render inputs for every database
// resource on a server, keyed by resource id. Mirrors DeployTargetsForServer.
func (s *Store) DBTargetsForServer(ctx context.Context, serverID string) (map[string]DBTarget, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT dc.resource_id, dc.engine, dc.username, dc.dbname, dc.port, sv.type,
		       COALESCE(bp.pitr_enabled, FALSE)
		  FROM db_credentials dc
		  JOIN servers sv ON sv.id = dc.server_id
		  LEFT JOIN backup_policies bp ON bp.resource_id = dc.resource_id
		 WHERE dc.server_id = $1`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]DBTarget{}
	for rows.Next() {
		var id string
		var t DBTarget
		if err := rows.Scan(&id, &t.Engine, &t.Username, &t.Database, &t.Port, &t.ServerType, &t.PITR); err != nil {
			return nil, err
		}
		out[id] = t
	}
	return out, rows.Err()
}

// BackupPolicy is a database's backup schedule and retention (P1-10 writes the
// row; P1-11 executes it).
type BackupPolicy struct {
	ID          string  `json:"id"`
	ResourceID  string  `json:"resourceId"`
	Schedule    string  `json:"schedule"`
	KeepDaily   int     `json:"keepDaily"`
	KeepWeekly  int     `json:"keepWeekly"`
	KeepMonthly int     `json:"keepMonthly"`
	TargetID    *string `json:"targetId"`
	Enabled     bool    `json:"enabled"`
	// PitrEnabled (P2-5, postgres only): continuous WAL archiving + a daily
	// physical base backup, the ingredients of a point-in-time restore.
	PitrEnabled bool `json:"pitrEnabled"`
}

// DatabaseInfo is a database resource's NON-secret connection metadata plus its
// backup policy — safe for any org member (Developer+) to read.
type DatabaseInfo struct {
	ResourceID string        `json:"resourceId"`
	Engine     string        `json:"engine"`
	Image      string        `json:"image"`
	Host       string        `json:"host"` // mesh IP; empty until mesh enrollment
	Port       int           `json:"port"`
	Database   string        `json:"database"`
	Username   string        `json:"username"`
	MeshOnly   bool          `json:"meshOnly"` // always true in v1
	Backup     *BackupPolicy `json:"backupPolicy,omitempty"`
	// P2-5 WAL archiving health: the PITR window honestly reaches only as far
	// as the last shipped segment. Nil = never shipped (or PITR off).
	LastWalAt      *time.Time `json:"lastWalAt,omitempty"`
	LastWalSegment string     `json:"lastWalSegment,omitempty"`
}

// DatabaseConnection is the audited reveal: DatabaseInfo plus the decrypted
// password and canonical connection URL. Project Admin+ only.
type DatabaseConnection struct {
	DatabaseInfo
	Password string `json:"password"`
	URL      string `json:"url"`
}

// dbInfo loads the non-secret half of a database's connection metadata.
func (s *Store) dbInfo(ctx context.Context, q pgxQuerier, orgID, resourceID string) (DatabaseInfo, string, string, []byte, []byte, error) {
	var (
		info   DatabaseInfo
		meshIP *string
		dekID  string
		ct     []byte
		nonce  []byte
	)
	err := q.QueryRow(ctx, `
		SELECT dc.resource_id, dc.engine, dc.username, dc.dbname, dc.port, dc.dek_id, dc.ciphertext, dc.nonce, sv.mesh_ip
		  FROM db_credentials dc
		  JOIN servers sv ON sv.id = dc.server_id
		 WHERE dc.org_id = $1 AND dc.resource_id = $2`,
		orgID, resourceID).Scan(&info.ResourceID, &info.Engine, &info.Username, &info.Database,
		&info.Port, &dekID, &ct, &nonce, &meshIP)
	if errors.Is(err, pgx.ErrNoRows) {
		return DatabaseInfo{}, "", "", nil, nil, ErrNotDatabase
	}
	if err != nil {
		return DatabaseInfo{}, "", "", nil, nil, err
	}
	if meshIP != nil {
		info.Host = *meshIP
	}
	info.MeshOnly = true
	if def, ok := DBEngine(info.Engine); ok {
		info.Image = def.Image
	}
	projectID := ""
	if err := q.QueryRow(ctx,
		`SELECT project_id FROM resources WHERE org_id = $1 AND id = $2`, orgID, resourceID).Scan(&projectID); err != nil {
		return DatabaseInfo{}, "", "", nil, nil, err
	}
	return info, projectID, dekID, ct, nonce, nil
}

// GetDatabaseInfo returns a database's non-secret connection metadata and
// backup policy. No reveal, no audit — Developer-visible.
func (s *Store) GetDatabaseInfo(ctx context.Context, orgID, resourceID string) (DatabaseInfo, error) {
	info, _, _, _, _, err := s.dbInfo(ctx, s.Pool, orgID, resourceID)
	if err != nil {
		return DatabaseInfo{}, err
	}
	var bp BackupPolicy
	err = s.Pool.QueryRow(ctx, `
		SELECT id, resource_id, schedule, keep_daily, keep_weekly, keep_monthly, target_id, enabled, pitr_enabled
		  FROM backup_policies WHERE org_id = $1 AND resource_id = $2`,
		orgID, resourceID).Scan(&bp.ID, &bp.ResourceID, &bp.Schedule, &bp.KeepDaily, &bp.KeepWeekly, &bp.KeepMonthly, &bp.TargetID, &bp.Enabled, &bp.PitrEnabled)
	if err == nil {
		info.Backup = &bp
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return DatabaseInfo{}, err
	}
	if bp.PitrEnabled {
		var lastAt *time.Time
		var lastSeg string
		err := s.Pool.QueryRow(ctx, `
			SELECT last_shipped_at, last_segment FROM wal_archive_status
			 WHERE org_id = $1 AND resource_id = $2`, orgID, resourceID).Scan(&lastAt, &lastSeg)
		if err == nil {
			info.LastWalAt = lastAt
			info.LastWalSegment = lastSeg
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return DatabaseInfo{}, err
		}
	}
	return info, nil
}

// RevealDatabaseConnection decrypts and returns a database's credentials and
// connection URL, writing an audit row (the reveal invariant: Developer 403s
// at the API layer; every successful reveal is audited).
func (s *Store) RevealDatabaseConnection(ctx context.Context, orgID, resourceID, actor string) (DatabaseConnection, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return DatabaseConnection{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	info, _, dekID, ct, nonce, err := s.dbInfo(ctx, tx, orgID, resourceID)
	if err != nil {
		return DatabaseConnection{}, err
	}
	dek, err := s.dekPlaintext(ctx, tx, dekID)
	if err != nil {
		return DatabaseConnection{}, err
	}
	plaintext, err := gcmOpen(dek, dbAAD(orgID, resourceID), nonce, ct)
	if err != nil {
		return DatabaseConnection{}, fmt.Errorf("decrypt db credentials: %w", err)
	}
	var creds dbCredentialsJSON
	if err := json.Unmarshal(plaintext, &creds); err != nil {
		return DatabaseConnection{}, err
	}
	if err := auditTx(ctx, tx, orgID, actor, "DB credentials revealed", resourceID); err != nil {
		return DatabaseConnection{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return DatabaseConnection{}, err
	}
	def, _ := DBEngine(info.Engine)
	return DatabaseConnection{
		DatabaseInfo: info,
		Password:     creds.Password,
		URL:          def.ConnectionURL(info.Username, creds.Password, info.Host, info.Port, info.Database),
	}, nil
}

// resolveDBSecretsTx appends a database resource's generated credentials as
// env-mode resolved secrets (names matching DBEngineDef.SecretEnvNames), for
// the agent's container-create injection. Returns nil, nil when the resource
// has no db_credentials row (not a database).
func (s *Store) resolveDBSecretsTx(ctx context.Context, tx pgx.Tx, orgID, serverID, resourceID string) ([]ResolvedSecret, error) {
	var (
		engine, dekID string
		ct, nonce     []byte
	)
	// server_id scoping mirrors ResolveSecretsForResource: an agent token only
	// ever drains credentials for resources scheduled onto ITS server.
	err := tx.QueryRow(ctx, `
		SELECT engine, dek_id, ciphertext, nonce
		  FROM db_credentials
		 WHERE org_id = $1 AND resource_id = $2 AND server_id = $3`,
		orgID, resourceID, serverID).Scan(&engine, &dekID, &ct, &nonce)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	dek, err := s.dekPlaintext(ctx, tx, dekID)
	if err != nil {
		return nil, err
	}
	plaintext, err := gcmOpen(dek, dbAAD(orgID, resourceID), nonce, ct)
	if err != nil {
		return nil, fmt.Errorf("decrypt db credentials: %w", err)
	}
	var creds dbCredentialsJSON
	if err := json.Unmarshal(plaintext, &creds); err != nil {
		return nil, err
	}
	var out []ResolvedSecret
	switch engine {
	case "postgres":
		out = append(out, ResolvedSecret{Name: "POSTGRES_PASSWORD", Value: creds.Password, EnvVar: true})
	case "mysql":
		out = append(out,
			ResolvedSecret{Name: "MYSQL_PASSWORD", Value: creds.Password, EnvVar: true},
			ResolvedSecret{Name: "MYSQL_ROOT_PASSWORD", Value: creds.RootPassword, EnvVar: true})
	case "redis":
		out = append(out, ResolvedSecret{Name: "REDIS_PASSWORD", Value: creds.Password, EnvVar: true})
	case "mongodb":
		out = append(out, ResolvedSecret{Name: "MONGO_INITDB_ROOT_PASSWORD", Value: creds.Password, EnvVar: true})
	}
	return out, nil
}
