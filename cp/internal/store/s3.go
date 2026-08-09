package store

// P2-1 S3 storage resources: the MinIO engine rides the exact P1-10 database
// pattern — CP-generated root credentials under the org-DEK envelope, a
// mesh-only host port, and a generic container render (the agent has zero
// S3-specific code). v1 scope is the service itself: buckets are managed with
// any S3 client using the revealed credentials; in-dashboard bucket/key CRUD
// is a follow-up (it needs a typed agent op).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// S3Kind is the single object-storage resource kind. The concrete engine
// (MinIO or SeaweedFS) is a per-resource selection under it, not a distinct
// kind — mirroring how a database "kind" already names its engine, but S3 needs
// engine choice under one semantic category (P2-2). The engines themselves are
// catalogued in s3_engines.go.
const S3Kind = "s3"

// IsS3Kind reports whether the kind is CP-provisioned object storage.
func IsS3Kind(kind string) bool { return kind == S3Kind }

// s3EngineFromSpec reads the selected engine from an s3 resource's spec,
// defaulting to MinIO when unspecified — backward-compatible with pre-P2-2
// resources whose spec carries no engine field.
func s3EngineFromSpec(spec json.RawMessage) string {
	if len(bytes.TrimSpace(spec)) == 0 {
		return DefaultS3Engine
	}
	var s struct {
		Engine string `json:"engine"`
	}
	if err := json.Unmarshal(spec, &s); err != nil || s.Engine == "" {
		return DefaultS3Engine
	}
	return s.Engine
}

// SetEnabledS3Engines installs the S3 engine allowlist (CP_S3_ENGINES),
// mirroring SetEnabledDBEngines: disabling an engine only gates NEW resource
// creation here. A nil allowlist enables every engine.
func (s *Store) SetEnabledS3Engines(engines []string) {
	m := map[string]bool{}
	for _, e := range engines {
		m[strings.TrimSpace(e)] = true
	}
	s.enabledS3Engines = m
}

// s3EngineEnabled reports whether new s3 resources of this engine may be created.
func (s *Store) s3EngineEnabled(engine string) bool {
	if s.enabledS3Engines == nil {
		return true
	}
	return s.enabledS3Engines[engine]
}

// s3AAD binds the credential ciphertext to its row identity.
func s3AAD(orgID, resourceID string) []byte { return []byte(orgID + "|s3|" + resourceID) }

type s3CredentialsJSON struct {
	SecretKey string `json:"secretKey"`
}

// ErrNotS3 marks a resource without S3 credentials (wrong kind / wrong org).
var ErrNotS3 = errors.New("resource is not an s3 storage")

// allocateS3Port shares the per-server mesh-port space with databases: same
// advisory lock, MAX over both tables, so an S3 resource can never collide
// with a database port on the same host.
func allocateS3Port(ctx context.Context, tx pgx.Tx, serverID string) (int, error) {
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext('dbport:' || $1))`, serverID); err != nil {
		return 0, fmt.Errorf("s3 port lock: %w", err)
	}
	var port int
	err := tx.QueryRow(ctx, `
		SELECT GREATEST(
			COALESCE((SELECT MAX(port) FROM db_credentials WHERE server_id = $1), $2 - 1),
			COALESCE((SELECT MAX(port) FROM s3_credentials WHERE server_id = $1), $2 - 1),
			COALESCE((SELECT MAX(port) FROM llm_endpoints WHERE server_id = $1), $2 - 1)
		) + 1`, serverID, MeshPortBase).Scan(&port)
	if err != nil {
		return 0, fmt.Errorf("s3 port max: %w", err)
	}
	return port, nil
}

// provisionS3Tx generates and stores an S3 resource's root credentials and
// mesh port inside CreateResource's transaction. No backup-policy row: the
// P1-11 dump/restore path is database-engine-specific, and pretending an S3
// store is covered by it would be fake safety (object-store DR is Phase 4).
func (s *Store) provisionS3Tx(ctx context.Context, tx pgx.Tx, orgID string, r Resource, engine string) error {
	def, ok := S3EngineByName(engine)
	if !ok {
		return fmt.Errorf("provision s3: unknown engine %q", engine)
	}
	port, err := allocateS3Port(ctx, tx, r.ServerID)
	if err != nil {
		return err
	}
	plaintext, err := json.Marshal(s3CredentialsJSON{SecretKey: randomDBSecret()})
	if err != nil {
		return err
	}
	dekID, dek, err := s.activeDEKTx(ctx, tx, orgID)
	if err != nil {
		return err
	}
	nonce, ct, err := gcmSeal(dek, s3AAD(orgID, r.ID), plaintext)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO s3_credentials (resource_id, org_id, server_id, engine, access_key, port, ciphertext, nonce, dek_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		r.ID, orgID, r.ServerID, def.Engine, "sigma", port, ct, nonce, dekID); err != nil {
		return fmt.Errorf("insert s3 credentials: %w", err)
	}
	return nil
}

// S3Target is the reconciler's render input for one S3 resource; the secret
// key never rides here.
type S3Target struct {
	Engine     string
	AccessKey  string
	Port       int
	ServerType string
}

// S3TargetsForServer returns the S3 resources scheduled on a server, keyed by
// resource id (mirrors DBTargetsForServer).
func (s *Store) S3TargetsForServer(ctx context.Context, serverID string) (map[string]S3Target, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT sc.resource_id, sc.engine, sc.access_key, sc.port, sv.type
		  FROM s3_credentials sc JOIN servers sv ON sv.id = sc.server_id
		 WHERE sc.server_id = $1`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]S3Target{}
	for rows.Next() {
		var resourceID string
		var t S3Target
		if err := rows.Scan(&resourceID, &t.Engine, &t.AccessKey, &t.Port, &t.ServerType); err != nil {
			return nil, err
		}
		out[resourceID] = t
	}
	return out, rows.Err()
}

// S3Info is the Developer-visible metadata (no secret key).
type S3Info struct {
	ResourceID string `json:"resourceId"`
	Engine     string `json:"engine"`
	Image      string `json:"image"`
	AccessKey  string `json:"accessKey"`
	// Host is the server's mesh IP; empty until mesh enrollment completes.
	Host     string `json:"host"`
	Port     int    `json:"port"`
	MeshOnly bool   `json:"meshOnly"`
	// Endpoint is the S3 API URL on the mesh ("" until the host has one).
	Endpoint string `json:"endpoint"`
}

// S3Connection adds the decrypted secret key (Project Admin reveal, audited).
type S3Connection struct {
	S3Info
	SecretKey string `json:"secretKey"`
}

func (s *Store) s3Info(ctx context.Context, q pgxQuerier, orgID, resourceID string) (S3Info, string, []byte, []byte, error) {
	var info S3Info
	var meshIP *string
	var dekID string
	var ct, nonce []byte
	err := q.QueryRow(ctx, `
		SELECT sc.resource_id, sc.engine, sc.access_key, sc.port, sv.mesh_ip, sc.dek_id, sc.ciphertext, sc.nonce
		  FROM s3_credentials sc JOIN servers sv ON sv.id = sc.server_id
		 WHERE sc.org_id = $1 AND sc.resource_id = $2`,
		orgID, resourceID).Scan(&info.ResourceID, &info.Engine, &info.AccessKey, &info.Port, &meshIP, &dekID, &ct, &nonce)
	if errors.Is(err, pgx.ErrNoRows) {
		return S3Info{}, "", nil, nil, ErrNotS3
	}
	if err != nil {
		return S3Info{}, "", nil, nil, err
	}
	def, _ := S3EngineByName(info.Engine)
	info.Image = def.Image
	info.MeshOnly = true
	if meshIP != nil {
		info.Host = *meshIP
	}
	info.Endpoint = def.EndpointURL(info.Host, info.Port)
	return info, dekID, ct, nonce, nil
}

// GetS3Info returns non-secret S3 metadata (Developer+).
func (s *Store) GetS3Info(ctx context.Context, orgID, resourceID string) (S3Info, error) {
	info, _, _, _, err := s.s3Info(ctx, s.Pool, orgID, resourceID)
	return info, err
}

// RevealS3Connection decrypts the root secret key. Audited.
func (s *Store) RevealS3Connection(ctx context.Context, orgID, resourceID, actor string) (S3Connection, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return S3Connection{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	info, dekID, ct, nonce, err := s.s3Info(ctx, tx, orgID, resourceID)
	if err != nil {
		return S3Connection{}, err
	}
	dek, err := s.dekPlaintext(ctx, tx, dekID)
	if err != nil {
		return S3Connection{}, err
	}
	plaintext, err := gcmOpen(dek, s3AAD(orgID, resourceID), nonce, ct)
	if err != nil {
		return S3Connection{}, fmt.Errorf("decrypt s3 credentials: %w", err)
	}
	var creds s3CredentialsJSON
	if err := json.Unmarshal(plaintext, &creds); err != nil {
		return S3Connection{}, err
	}
	if err := auditTx(ctx, tx, orgID, actor, "S3 credentials revealed", resourceID); err != nil {
		return S3Connection{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return S3Connection{}, err
	}
	return S3Connection{S3Info: info, SecretKey: creds.SecretKey}, nil
}

// resolveS3SecretsTx appends an S3 resource's root password as an env-mode
// resolved secret for the agent's container-create injection. nil, nil when
// the resource has no s3_credentials row.
func (s *Store) resolveS3SecretsTx(ctx context.Context, tx pgx.Tx, orgID, serverID, resourceID string) ([]ResolvedSecret, error) {
	var engine, dekID string
	var ct, nonce []byte
	err := tx.QueryRow(ctx, `
		SELECT engine, dek_id, ciphertext, nonce
		  FROM s3_credentials
		 WHERE org_id = $1 AND resource_id = $2 AND server_id = $3`,
		orgID, resourceID, serverID).Scan(&engine, &dekID, &ct, &nonce)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	def, ok := S3EngineByName(engine)
	if !ok {
		return nil, fmt.Errorf("resolve s3 secrets: unknown engine %q", engine)
	}
	dek, err := s.dekPlaintext(ctx, tx, dekID)
	if err != nil {
		return nil, err
	}
	plaintext, err := gcmOpen(dek, s3AAD(orgID, resourceID), nonce, ct)
	if err != nil {
		return nil, fmt.Errorf("decrypt s3 credentials: %w", err)
	}
	var creds s3CredentialsJSON
	if err := json.Unmarshal(plaintext, &creds); err != nil {
		return nil, err
	}
	// The secret key rides under whatever env var(s) the engine reads it from
	// (MINIO_ROOT_PASSWORD for MinIO, AWS_SECRET_ACCESS_KEY for SeaweedFS).
	out := make([]ResolvedSecret, 0, len(def.SecretEnvNames))
	for _, name := range def.SecretEnvNames {
		out = append(out, ResolvedSecret{Name: name, Value: creds.SecretKey, EnvVar: true})
	}
	return out, nil
}
