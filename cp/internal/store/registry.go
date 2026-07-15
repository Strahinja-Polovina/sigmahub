package store

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	// ErrTokenInvalid covers unknown, expired and already-used bootstrap
	// tokens — callers must not be able to distinguish which it was.
	ErrTokenInvalid = errors.New("bootstrap token invalid")
	ErrNotFound     = errors.New("not found")
)

type Server struct {
	ID           string          `json:"id"`
	OrgID        string          `json:"orgId"`
	Name         string          `json:"name"`
	Type         string          `json:"type"`
	Source       string          `json:"source"`
	ProxyRole    bool            `json:"proxyRole"`
	Provider     string          `json:"provider"`
	Region       string          `json:"region"`
	Status       string          `json:"status"`
	AgentVersion string          `json:"agentVersion"`
	Facts        json.RawMessage `json:"facts"`
	MeshIP       *string         `json:"meshIp"`
	Pubkey       *string         `json:"pubkey"`
	LastSeenAt   *time.Time      `json:"lastSeenAt"`
	CreatedAt    time.Time       `json:"createdAt"`
}

func newID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}

// newToken returns (plaintext, digest). Plaintext is shown exactly once; only
// the keyed digest is persisted.
func (s *Store) newToken(prefix string) (string, []byte) {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	tok := prefix + "_" + hex.EncodeToString(b)
	return tok, s.hashToken(tok)
}

// hashToken keys the token digest with the KMS-custodied pepper (HMAC-SHA256),
// so a database leak alone can't be brute-forced into valid tokens without the
// wrapped key material. Pre-pepper deployments fall back to plain SHA-256.
func (s *Store) hashToken(tok string) []byte {
	if len(s.pepper) == 0 {
		sum := sha256.Sum256([]byte(tok))
		return sum[:]
	}
	mac := hmac.New(sha256.New, s.pepper)
	mac.Write([]byte(tok))
	return mac.Sum(nil)
}

// normalizeFacts guarantees the facts column always holds a JSON object.
// Anything else — empty, JSON null, a scalar or an array — collapses to {},
// so readers and `facts->>'key'` filters never hit a non-object.
func normalizeFacts(facts json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(facts)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return json.RawMessage(`{}`)
	}
	return trimmed
}

// IssueBootstrapToken creates a single-use, expiring registration token for
// an org and audits the issuance. The plaintext is returned once and never
// stored.
func (s *Store) IssueBootstrapToken(ctx context.Context, orgID, serverName, serverType, provider, region, createdBy string, ttl time.Duration) (token string, expiresAt time.Time, err error) {
	tok, digest := s.newToken("sbt")
	expiresAt = time.Now().Add(ttl)

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return "", time.Time{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		INSERT INTO bootstrap_tokens (id, org_id, token_hash, server_name, server_type, provider, region, created_by, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		newID("bt"), orgID, digest, serverName, serverType, provider, region, createdBy, expiresAt); err != nil {
		return "", time.Time{}, fmt.Errorf("insert bootstrap token: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO cp_audit_log (org_id, actor, action, target)
		VALUES ($1, $2, 'Bootstrap token issued', $3)`,
		orgID, createdBy, serverName); err != nil {
		return "", time.Time{}, fmt.Errorf("audit: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", time.Time{}, err
	}
	return tok, expiresAt, nil
}

type RegisterResult struct {
	Server     Server
	AgentToken string
}

// RegisterServer atomically claims a bootstrap token (single-use enforced by
// the conditional UPDATE), creates the server record and issues its agent
// token, all in one transaction.
func (s *Store) RegisterServer(ctx context.Context, bootstrapToken, name, agentVersion string, facts json.RawMessage, pubkey string) (RegisterResult, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return RegisterResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		orgID, tokenID       string
		tokName, tokType     string
		tokProvider, tokRegion string
	)
	// Single-use claim: only one concurrent register can win this UPDATE.
	err = tx.QueryRow(ctx, `
		UPDATE bootstrap_tokens
		   SET used_at = now()
		 WHERE token_hash = $1 AND used_at IS NULL AND expires_at > now()
		 RETURNING id, org_id, server_name, server_type, provider, region`,
		s.hashToken(bootstrapToken),
	).Scan(&tokenID, &orgID, &tokName, &tokType, &tokProvider, &tokRegion)
	if errors.Is(err, pgx.ErrNoRows) {
		return RegisterResult{}, ErrTokenInvalid
	}
	if err != nil {
		return RegisterResult{}, fmt.Errorf("claim bootstrap token: %w", err)
	}

	serverName := tokName
	if serverName == "" {
		serverName = name
	}
	if serverName == "" {
		serverName = "server"
	}
	facts = normalizeFacts(facts)

	meshIP, err := allocateMeshIP(ctx, tx, orgID)
	if err != nil {
		return RegisterResult{}, err
	}

	srv := Server{ID: newID("srv")}
	err = tx.QueryRow(ctx, `
		INSERT INTO servers (id, org_id, name, type, provider, region, agent_version, facts, pubkey, mesh_ip)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULLIF($9, ''), $10)
		RETURNING id, org_id, name, type, source, proxy_role, provider, region, status, agent_version, facts, mesh_ip, pubkey, last_seen_at, created_at`,
		srv.ID, orgID, serverName, tokType, tokProvider, tokRegion, agentVersion, facts, pubkey, meshIP,
	).Scan(&srv.ID, &srv.OrgID, &srv.Name, &srv.Type, &srv.Source, &srv.ProxyRole, &srv.Provider, &srv.Region,
		&srv.Status, &srv.AgentVersion, &srv.Facts, &srv.MeshIP, &srv.Pubkey, &srv.LastSeenAt, &srv.CreatedAt)
	if err != nil {
		return RegisterResult{}, fmt.Errorf("insert server: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE bootstrap_tokens SET server_id = $1 WHERE id = $2`, srv.ID, tokenID); err != nil {
		return RegisterResult{}, fmt.Errorf("link token: %w", err)
	}

	agentTok, digest := s.newToken("sat")
	if _, err := tx.Exec(ctx,
		`INSERT INTO agent_tokens (id, server_id, token_hash) VALUES ($1, $2, $3)`,
		newID("at"), srv.ID, digest); err != nil {
		return RegisterResult{}, fmt.Errorf("insert agent token: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO cp_audit_log (org_id, actor, action, target)
		VALUES ($1, 'sigmad', 'Server registered', $2)`, orgID, srv.Name); err != nil {
		return RegisterResult{}, fmt.Errorf("audit: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return RegisterResult{}, err
	}
	return RegisterResult{Server: srv, AgentToken: agentTok}, nil
}

// ServerByAgentToken authenticates an agent credential and returns its server.
func (s *Store) ServerByAgentToken(ctx context.Context, token string) (Server, error) {
	var srv Server
	err := s.Pool.QueryRow(ctx, `
		SELECT s.id, s.org_id, s.name, s.type, s.source, s.proxy_role, s.provider, s.region, s.status,
		       s.agent_version, s.facts, s.mesh_ip, s.pubkey, s.last_seen_at, s.created_at
		  FROM agent_tokens t
		  JOIN servers s ON s.id = t.server_id
		 WHERE t.token_hash = $1 AND t.revoked_at IS NULL`,
		s.hashToken(token),
	).Scan(&srv.ID, &srv.OrgID, &srv.Name, &srv.Type, &srv.Source, &srv.ProxyRole, &srv.Provider, &srv.Region,
		&srv.Status, &srv.AgentVersion, &srv.Facts, &srv.MeshIP, &srv.Pubkey, &srv.LastSeenAt, &srv.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Server{}, ErrNotFound
	}
	if err != nil {
		return Server{}, err
	}
	return srv, nil
}

func (s *Store) ListServers(ctx context.Context, orgID string) ([]Server, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, org_id, name, type, source, proxy_role, provider, region, status,
		       agent_version, facts, mesh_ip, pubkey, last_seen_at, created_at
		  FROM servers WHERE org_id = $1 ORDER BY created_at`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Server
	for rows.Next() {
		var srv Server
		if err := rows.Scan(&srv.ID, &srv.OrgID, &srv.Name, &srv.Type, &srv.Source, &srv.ProxyRole, &srv.Provider, &srv.Region,
			&srv.Status, &srv.AgentVersion, &srv.Facts, &srv.MeshIP, &srv.Pubkey, &srv.LastSeenAt, &srv.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, srv)
	}
	return out, rows.Err()
}

func (s *Store) GetServer(ctx context.Context, orgID, serverID string) (Server, error) {
	var srv Server
	err := s.Pool.QueryRow(ctx, `
		SELECT id, org_id, name, type, source, proxy_role, provider, region, status,
		       agent_version, facts, mesh_ip, pubkey, last_seen_at, created_at
		  FROM servers WHERE org_id = $1 AND id = $2`, orgID, serverID,
	).Scan(&srv.ID, &srv.OrgID, &srv.Name, &srv.Type, &srv.Source, &srv.ProxyRole, &srv.Provider, &srv.Region,
		&srv.Status, &srv.AgentVersion, &srv.Facts, &srv.MeshIP, &srv.Pubkey, &srv.LastSeenAt, &srv.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Server{}, ErrNotFound
	}
	return srv, err
}
