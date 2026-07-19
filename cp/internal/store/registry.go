package store

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
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

// supportedDistros is the exact set the SSH provisioner accepts; anything else
// is rejected up front with an actionable error (SIGMA-A-5: Ubuntu 22.04/24.04
// and Debian 12 only for the hardened onboarding path).
var supportedDistros = map[string]bool{
	"ubuntu-22.04": true,
	"ubuntu-24.04": true,
	"debian-12":    true,
}

// ErrUnsupportedDistro is returned when a host's OS is outside supportedDistros.
var ErrUnsupportedDistro = errors.New("unsupported distro")

// DistroSupported reports whether a normalized distro id is onboardable.
func DistroSupported(distro string) bool { return supportedDistros[distro] }

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
	// P1-5 dashboard fields. Ready is DERIVED on read (running + mesh applied +
	// a formable same-org peer); the rest are the reported hardening posture and
	// declared distro. Populated by ListServers/GetServer only.
	Distro         *string `json:"distro"`
	Ready          bool    `json:"ready"`
	MeshPeerCount  int     `json:"meshPeerCount"`
	HardeningScore *int    `json:"hardeningScore"`
	DiskEncrypted  *bool   `json:"diskEncrypted"`
	SSHLocked      *bool   `json:"sshLocked"`
}

// readinessExpr is the derived Ready predicate: the server is live (running),
// has applied its mesh config, and a same-org peer is dialable (has a pubkey +
// mesh IP + endpoint) so a tunnel can form. Computed on read against current peer
// rows, so it drops promptly when peers leave — no stale persisted flag.
const readinessExpr = `(
	s.status = 'running'
	AND s.mesh_synced_at IS NOT NULL
	AND s.pubkey IS NOT NULL AND s.mesh_ip IS NOT NULL
	AND EXISTS (
		SELECT 1 FROM servers p
		 WHERE p.org_id = s.org_id AND p.id <> s.id AND p.deleted_at IS NULL
		   AND p.pubkey IS NOT NULL AND p.mesh_ip IS NOT NULL AND p.endpoint IS NOT NULL
	)
)`

// randBytes returns n cryptographically-secure random bytes, panicking if the
// system CSPRNG fails. A broken RNG is unrecoverable, and every caller mints a
// credential or a row id — failing closed (a recovered 500) is strictly safer
// than silently minting a partially-predictable id/token, which is what
// `_, _ = rand.Read(b)` did (SIGMA-80). crypto/rand.Read never fails on Linux
// in practice, so this panics only on a genuinely broken host.
func randBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return b
}

func newID(prefix string) string {
	return prefix + "_" + hex.EncodeToString(randBytes(8))
}

// newToken returns (plaintext, digest). Plaintext is shown exactly once; only
// the keyed digest is persisted.
func (s *Store) newToken(prefix string) (string, []byte) {
	tok := prefix + "_" + hex.EncodeToString(randBytes(24))
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

// ProvisionInput describes a server to pre-create during onboarding.
type ProvisionInput struct {
	Name      string
	Type      string
	Provider  string
	Region    string
	ProxyRole bool
	Distro    string // "" for the manual/NAT path; validated when set
}

// precreateServerTx inserts a provisioning server row and allocates its mesh IP,
// returning the new id. The server exists BEFORE the agent registers, so the
// bootstrap token binds to a concrete record and the WireGuard config the
// installer applies can carry this server's mesh IP from the first heartbeat.
func precreateServerTx(ctx context.Context, tx pgx.Tx, orgID string, in ProvisionInput) (string, error) {
	if in.Distro != "" && !supportedDistros[in.Distro] {
		return "", ErrUnsupportedDistro
	}
	name := in.Name
	if name == "" {
		name = "server"
	}
	typ := in.Type
	if typ == "" {
		typ = "general"
	}
	meshIP, err := allocateMeshIP(ctx, tx, orgID)
	if err != nil {
		return "", err
	}
	id := newID("srv")
	if _, err := tx.Exec(ctx, `
		INSERT INTO servers (id, org_id, name, type, source, proxy_role, provider, region, mesh_ip, distro, status)
		VALUES ($1, $2, $3, $4, 'provisioned', $5, $6, $7, $8, NULLIF($9, ''), 'provisioning')`,
		id, orgID, name, typ, in.ProxyRole, in.Provider, in.Region, meshIP, in.Distro); err != nil {
		return "", fmt.Errorf("pre-create server: %w", err)
	}
	return id, nil
}

// issueTokenTx binds a fresh single-use bootstrap token to an already-created
// server and audits it. Returns the plaintext (shown once) and its digest row id.
func (s *Store) issueTokenTx(ctx context.Context, tx pgx.Tx, orgID, serverID, serverName, createdBy string, expiresAt time.Time) (string, error) {
	tok, digest := s.newToken("sbt")
	if _, err := tx.Exec(ctx, `
		INSERT INTO bootstrap_tokens (id, org_id, token_hash, server_id, server_name, server_type, provider, region, created_by, expires_at)
		VALUES ($1, $2, $3, $4, $5, '', '', '', $6, $7)`,
		newID("bt"), orgID, digest, serverID, serverName, createdBy, expiresAt); err != nil {
		return "", fmt.Errorf("insert bootstrap token: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO cp_audit_log (org_id, actor, action, target)
		VALUES ($1, $2, 'Bootstrap token issued', $3)`,
		orgID, createdBy, serverName); err != nil {
		return "", fmt.Errorf("audit: %w", err)
	}
	return tok, nil
}

// IssueBootstrapToken pre-creates the server record and returns a single-use,
// short-lived token bound to it. The plaintext is returned once and never
// stored. This is the manual/NAT path (no SSH keypair); ProvisionServer is the
// SSH path.
func (s *Store) IssueBootstrapToken(ctx context.Context, orgID, serverName, serverType, provider, region, createdBy string, ttl time.Duration) (token, serverID string, expiresAt time.Time, err error) {
	expiresAt = time.Now().Add(ttl)
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return "", "", time.Time{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	serverID, err = precreateServerTx(ctx, tx, orgID, ProvisionInput{
		Name: serverName, Type: serverType, Provider: provider, Region: region})
	if err != nil {
		return "", "", time.Time{}, err
	}
	token, err = s.issueTokenTx(ctx, tx, orgID, serverID, serverName, createdBy, expiresAt)
	if err != nil {
		return "", "", time.Time{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", "", time.Time{}, err
	}
	return token, serverID, expiresAt, nil
}

// ProvisionResult is returned by the SSH provisioner: the pre-created server, a
// bound bootstrap token, and the ed25519 public key (OpenSSH authorized_keys
// form) the provisioner drops onto the host for its one-time login.
type ProvisionResult struct {
	ServerID        string
	Token           string
	ExpiresAt       time.Time
	BootstrapPubkey string
}

// ProvisionServer is the SSH onboarding path: it pre-creates the server, mints a
// per-server ed25519 bootstrap keypair (the seed is KMS-wrapped and stored; the
// public key is returned so it can be appended to the host's authorized_keys for
// a single login, then removed by the installer), and issues a bound token.
func (s *Store) ProvisionServer(ctx context.Context, orgID string, in ProvisionInput, createdBy string, ttl time.Duration) (ProvisionResult, error) {
	if in.Distro != "" && !supportedDistros[in.Distro] {
		return ProvisionResult{}, ErrUnsupportedDistro
	}
	pub, seed, err := generateEd25519Seed()
	if err != nil {
		return ProvisionResult{}, err
	}
	wrapped, err := s.custody.Wrap(ctx, "srv_bootstrap:"+orgID, seed)
	if err != nil {
		return ProvisionResult{}, fmt.Errorf("wrap bootstrap key: %w", err)
	}
	authorizedKey := sshEd25519AuthorizedKey(pub)

	expiresAt := time.Now().Add(ttl)
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return ProvisionResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	serverID, err := precreateServerTx(ctx, tx, orgID, in)
	if err != nil {
		return ProvisionResult{}, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE servers SET bootstrap_pubkey = $1, bootstrap_key_wrapped = $2 WHERE id = $3`,
		authorizedKey, wrapped, serverID); err != nil {
		return ProvisionResult{}, fmt.Errorf("store bootstrap key: %w", err)
	}
	name := in.Name
	if name == "" {
		name = "server"
	}
	token, err := s.issueTokenTx(ctx, tx, orgID, serverID, name, createdBy, expiresAt)
	if err != nil {
		return ProvisionResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ProvisionResult{}, err
	}
	return ProvisionResult{ServerID: serverID, Token: token, ExpiresAt: expiresAt, BootstrapPubkey: authorizedKey}, nil
}

// generateEd25519Seed returns a new ed25519 public key and its 32-byte seed
// (the wrappable private half).
func generateEd25519Seed() (ed25519.PublicKey, []byte, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	return pub, priv.Seed(), nil
}

// sshEd25519AuthorizedKey formats an ed25519 public key as an OpenSSH
// authorized_keys line (wire format: string "ssh-ed25519" + string(pubkey),
// base64). Hand-rolled to avoid pulling in golang.org/x/crypto/ssh.
func sshEd25519AuthorizedKey(pub ed25519.PublicKey) string {
	var buf bytes.Buffer
	writeSSHString(&buf, []byte("ssh-ed25519"))
	writeSSHString(&buf, pub)
	return "ssh-ed25519 " + base64.StdEncoding.EncodeToString(buf.Bytes()) + " sigmahub-bootstrap"
}

func writeSSHString(buf *bytes.Buffer, b []byte) {
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(b)))
	buf.Write(l[:])
	buf.Write(b)
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

	var orgID, serverID string
	// Single-use claim: only one concurrent register can win this UPDATE. The
	// token is bound to a PRE-CREATED server (server_id), so we update that row
	// rather than inserting a new one.
	err = tx.QueryRow(ctx, `
		UPDATE bootstrap_tokens
		   SET used_at = now()
		 WHERE token_hash = $1 AND used_at IS NULL AND expires_at > now()
		 RETURNING org_id, server_id`,
		s.hashToken(bootstrapToken),
	).Scan(&orgID, &serverID)
	if errors.Is(err, pgx.ErrNoRows) {
		return RegisterResult{}, ErrTokenInvalid
	}
	if err != nil {
		return RegisterResult{}, fmt.Errorf("claim bootstrap token: %w", err)
	}
	if serverID == "" {
		// A token with no bound server is unusable (should not happen post-0012).
		return RegisterResult{}, ErrTokenInvalid
	}
	facts = normalizeFacts(facts)

	// Populate the pre-created row with what the agent reports at first contact.
	// Name/type/provider/region/mesh_ip were set at provision time and are kept.
	var srv Server
	err = tx.QueryRow(ctx, `
		UPDATE servers
		   SET agent_version = $3, facts = $4, pubkey = NULLIF($5, '')
		 WHERE id = $1 AND org_id = $2 AND deleted_at IS NULL
		 RETURNING id, org_id, name, type, source, proxy_role, provider, region, status, agent_version, facts, mesh_ip, pubkey, last_seen_at, created_at`,
		serverID, orgID, agentVersion, facts, pubkey,
	).Scan(&srv.ID, &srv.OrgID, &srv.Name, &srv.Type, &srv.Source, &srv.ProxyRole, &srv.Provider, &srv.Region,
		&srv.Status, &srv.AgentVersion, &srv.Facts, &srv.MeshIP, &srv.Pubkey, &srv.LastSeenAt, &srv.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// Server deleted between provision and register.
		return RegisterResult{}, ErrTokenInvalid
	}
	if err != nil {
		return RegisterResult{}, fmt.Errorf("update server: %w", err)
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
		 WHERE t.token_hash = $1 AND t.revoked_at IS NULL AND s.deleted_at IS NULL`,
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

// serverSelect is the dashboard-facing projection: the base columns plus the
// declared distro, the reported hardening posture, mesh peer count, and the
// derived Ready flag. Aliased `s` so readinessExpr's correlated subquery binds.
const serverSelect = `
	SELECT s.id, s.org_id, s.name, s.type, s.source, s.proxy_role, s.provider, s.region, s.status,
	       s.agent_version, s.facts, s.mesh_ip, s.pubkey, s.last_seen_at, s.created_at,
	       s.distro, s.mesh_peer_count, s.hardening_score, s.disk_encrypted, s.ssh_locked, ` + readinessExpr + ` AS ready
	  FROM servers s`

func scanServerRow(row pgx.Row, srv *Server) error {
	return row.Scan(&srv.ID, &srv.OrgID, &srv.Name, &srv.Type, &srv.Source, &srv.ProxyRole, &srv.Provider, &srv.Region,
		&srv.Status, &srv.AgentVersion, &srv.Facts, &srv.MeshIP, &srv.Pubkey, &srv.LastSeenAt, &srv.CreatedAt,
		&srv.Distro, &srv.MeshPeerCount, &srv.HardeningScore, &srv.DiskEncrypted, &srv.SSHLocked, &srv.Ready)
}

func (s *Store) ListServers(ctx context.Context, orgID string) ([]Server, error) {
	rows, err := s.Pool.Query(ctx, serverSelect+` WHERE s.org_id = $1 AND s.deleted_at IS NULL ORDER BY s.created_at`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Server
	for rows.Next() {
		var srv Server
		if err := scanServerRow(rows, &srv); err != nil {
			return nil, err
		}
		out = append(out, srv)
	}
	return out, rows.Err()
}

func (s *Store) GetServer(ctx context.Context, orgID, serverID string) (Server, error) {
	var srv Server
	err := scanServerRow(
		s.Pool.QueryRow(ctx, serverSelect+` WHERE s.org_id = $1 AND s.id = $2 AND s.deleted_at IS NULL`, orgID, serverID),
		&srv)
	if errors.Is(err, pgx.ErrNoRows) {
		return Server{}, ErrNotFound
	}
	return srv, err
}
