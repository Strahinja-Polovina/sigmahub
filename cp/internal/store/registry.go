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
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	// ErrTokenInvalid covers unknown, expired and already-used bootstrap
	// tokens — callers must not be able to distinguish which it was.
	ErrTokenInvalid = errors.New("bootstrap token invalid")
	ErrNotFound     = errors.New("not found")
)

// ErrUnsupportedDistro is returned when a host's OS is outside the onboardable
// set (SIGMA-A-5: the hardened path accepts Ubuntu 22.04/24.04 and Debian 12).
// The set itself, and each server type's own narrower allow-list, live in
// server_catalog.go — see DistroSupported / SupportedDistroSentence.
var ErrUnsupportedDistro = errors.New("unsupported distro")

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
	// Endpoint is the server's PUBLIC address (ip:port), from the connect
	// wizard initially and refreshed by the agent's STUN probe on heartbeat.
	// Surfaced so the dashboard can label it distinctly from the 10.8.x.x mesh
	// IP it previously presented as "IP" (SIGMA-187).
	Endpoint   *string    `json:"endpoint"`
	Pubkey     *string    `json:"pubkey"`
	LastSeenAt *time.Time `json:"lastSeenAt"`
	CreatedAt  time.Time  `json:"createdAt"`
	// P1-5 dashboard fields. Ready is DERIVED on read (running + mesh applied +
	// a formable same-org peer); the rest are the reported hardening posture and
	// declared distro. Populated by ListServers/GetServer only.
	Distro         *string `json:"distro"`
	Ready          bool    `json:"ready"`
	MeshPeerCount  int     `json:"meshPeerCount"`
	HardeningScore *int    `json:"hardeningScore"`
	DiskEncrypted  *bool   `json:"diskEncrypted"`
	SSHLocked      *bool   `json:"sshLocked"`
	// KeepPublicSSH is the CONFIGURED hardening intent (as opposed to SSHLocked,
	// which is the agent's reported posture). Surfaced so the dashboard can show
	// and change it after provisioning instead of it being write-once at connect
	// time (SIGMA-179). Defaults TRUE when no hardening row exists, matching
	// HostHardeningForServer's fail-safe.
	KeepPublicSSH bool `json:"keepPublicSsh"`
	// IncompatibleReasons is why status is 'incompatible' — one entry per
	// requirement of this server's TYPE that its reported facts violate
	// (SIGMA-203). Always an array, empty for every other status, so the
	// dashboard has no null case to invent a meaning for. Populated by
	// ListServers/GetServer only.
	IncompatibleReasons []FailedRequirement `json:"incompatibleReasons"`
	// DecommissioningSince is when a graceful decommission was asked for
	// (SIGMA-204), null otherwise. The dashboard needs the TIMESTAMP and not
	// just the status: "Force disconnect" is only offered once the graceful path
	// has had its chance, and the only way to know that without the CP inventing
	// a second flag is to compare this against the timeout.
	DecommissioningSince *time.Time `json:"decommissioningSince"`
	// PurgeVolumes echoes the operator's opt-in on the in-flight request, so a
	// reload of the page mid-teardown still says whether application data is
	// being destroyed.
	PurgeVolumes bool `json:"purgeVolumes"`
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

// zeroValuedFactKeys are the reported host facts whose zero value means "the
// probe could not answer" rather than a real reading — an agent that could not
// read /etc/os-release, or could not stat the data root. They are stripped
// before the facts are merged so a failed probe leaves the last known value
// standing instead of blanking it (SIGMA-201; see facts.go for the rules).
//
// Three keys are pointedly NOT in this list, because their zero IS the reading:
//
//   - `gpu` — an empty inventory is a real answer ("I looked, this host has no
//     GPU"), or a machine whose card was pulled advertises it forever.
//   - `diskFreeBytes` — zero free is exactly the state worth knowing about, and
//     it is reachable: gopsutil reports the unprivileged-available figure, which
//     hits 0 on ext4 while the root reserve still has room. Stripping it made a
//     FULL disk keep advertising its last healthy free-space figure forever.
//     The agent sends this one as a pointer, so "could not stat" is absent and
//     "genuinely zero" is 0 — a distinction `omitempty` on a plain uint64
//     cannot make.
//   - `dockerVersion` — because facts MERGE, a key that is merely omitted keeps
//     its old value. A host that had Docker removed reports
//     dockerAvailable:false and omits the version, so the dashboard went on
//     showing the version of a daemon that is no longer installed. An explicit
//     "" now clears it.
var zeroValuedFactKeys = []string{"distro", "distroName", "diskPath", "diskTotalBytes"}

// maxFactKeys and maxFactBytes bound one server's facts cell.
//
// Facts are merged, and a merge never removes a key: without a bound, an agent
// token holder can add one new key per 30-second heartbeat until the jsonb cell
// reaches Postgres' 1 GB field limit, after which that server can never
// heartbeat again — a self-inflicted denial of service on a host the operator
// owns. Assignment used to bound this implicitly; the merge that fixed version
// skew removed the bound, so it has to be stated.
const (
	maxFactKeys  = 64
	maxFactBytes = 16 << 10
)

// normalizeFacts guarantees the facts payload is a JSON object and strips the
// keys that carry no information.
//
// Anything that is not an object — empty, JSON null, a scalar, an array, or
// malformed JSON — collapses to {}, so readers and `facts->>'key'` filters
// never hit a non-object. Malformed JSON used to be passed straight through to
// the jsonb cast and fail the whole heartbeat; now the payload is simply empty,
// which, because facts are MERGED, leaves the stored facts intact.
//
// Values are re-emitted verbatim (they stay json.RawMessage), so this only ever
// removes keys — it never rewrites a reading.
func normalizeFacts(facts json.RawMessage) json.RawMessage {
	empty := json.RawMessage(`{}`)
	trimmed := bytes.TrimSpace(facts)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return empty
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return empty
	}
	// A JSON null is "no value" for ANY fact, not just the ones below: merging
	// it in would replace a real reading with a null that every reader then has
	// to special-case on top of the absent case it already handles.
	for key, raw := range obj {
		if string(bytes.TrimSpace(raw)) == "null" {
			delete(obj, key)
		}
	}
	for _, key := range zeroValuedFactKeys {
		if isZeroJSON(obj[key]) {
			delete(obj, key)
		}
	}
	// Refuse an oversized or over-wide payload outright rather than merging part
	// of it: a partial merge would be indistinguishable from a healthy check-in
	// while still growing the cell. Empty means "nothing to merge", so the
	// stored facts survive and the heartbeat itself still succeeds — a host is
	// not taken offline for sending junk.
	if len(obj) > maxFactKeys {
		return empty
	}
	out, err := json.Marshal(obj)
	if err != nil || len(out) > maxFactBytes {
		return empty
	}
	return out
}

// reportedHostname extracts the agent's hostname from a normalized facts blob,
// sanitized for use as a server NAME (SIGMA-202).
//
// Deliberately not a field on HostFacts: that struct is the typed view over
// exactly the facts the catalog's requirements read, and widening it for a
// value no requirement checks would blur the one rule that keeps the gate
// honest. This is a different question asked of the same blob.
//
// Sanitizing matters because whoever holds the bootstrap token controls this
// string, and it becomes a display name, an audit target and a search key. A
// hostname is at most 253 bytes of printable ASCII; anything else is either a
// broken host or someone trying to write a newline into an audit log.
func reportedHostname(facts json.RawMessage) string {
	var f struct {
		Hostname string `json:"hostname"`
	}
	if err := json.Unmarshal(facts, &f); err != nil {
		return ""
	}
	return sanitizeServerName(f.Hostname)
}

// maxServerNameLen is RFC 1035's limit on a full domain name. A server name can
// come from a hostname, so the two share a bound rather than the rename path
// inventing a shorter one that a legitimate auto-assigned name would fail.
const maxServerNameLen = 253

// sanitizeServerName trims, drops control characters and bounds the length.
func sanitizeServerName(name string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(name) {
		if r < ' ' || r == 0x7f {
			continue
		}
		if b.Len() >= maxServerNameLen {
			break
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// isZeroJSON reports whether a raw value is absent, null, "", 0 or false — the
// encodings a Go zero value takes on the wire when an agent serializes a field
// it could not fill in.
func isZeroJSON(raw json.RawMessage) bool {
	switch string(bytes.TrimSpace(raw)) {
	case "", "null", `""`, "0", "false":
		return true
	}
	return false
}

// ProvisionInput describes a server to pre-create during onboarding.
type ProvisionInput struct {
	// Name is OPTIONAL since SIGMA-202: the connect form asks for the host
	// address and the type, and nothing else. An empty name pre-creates the row
	// under a placeholder (the host address) and marks it machine-assigned, so
	// registration can replace it with the hostname the machine reports. A name
	// given here is the operator's and is never overwritten.
	Name      string
	Type      string
	Provider  string
	Region    string
	ProxyRole bool
	Distro    string // "" for the manual/NAT path; validated when set
	// HostIP is the public address the operator typed into the connect wizard.
	// Stored as the server's initial endpoint so the dashboard can show a real
	// public IP from day one; the agent's STUN probe refines it on heartbeat
	// (SIGMA-187 — previously this input was collected and silently discarded).
	HostIP string
}

// precreateServerTx inserts a provisioning server row and allocates its mesh IP,
// returning the new id. The server exists BEFORE the agent registers, so the
// bootstrap token binds to a concrete record and the WireGuard config the
// installer applies can carry this server's mesh IP from the first heartbeat.
// Returns the id AND the resolved name, because the name is no longer
// necessarily the caller's: an empty one becomes a placeholder here, and the
// bootstrap-token row and the audit entry have to record what the server is
// actually called rather than the empty string the caller passed.
func precreateServerTx(ctx context.Context, tx pgx.Tx, orgID string, in ProvisionInput) (string, string, error) {
	if in.Distro != "" && !DistroSupported(in.Distro) {
		return "", "", ErrUnsupportedDistro
	}
	// A name the operator did not give is a name the MACHINE gets to supply:
	// registration replaces the placeholder with the reported hostname
	// (SIGMA-202). Until the agent checks in the row still has to be
	// identifiable in a list, so the placeholder is the address the operator
	// typed — the only handle they have on the box at that moment. "server" is
	// the last resort for the manual/NAT path, which has no address either.
	name, nameAuto := in.Name, false
	if name == "" {
		nameAuto = true
		name = strings.TrimSpace(in.HostIP)
		if name == "" {
			name = "server"
		}
	}
	typ := in.Type
	if typ == "" {
		typ = "general"
	}
	// A typo'd type would enroll a host nothing can ever be scheduled onto (the
	// availability matrix would match no kind), and it would bill at the default
	// weight rather than its real one. Fail at enrollment instead.
	if !IsServerType(typ) {
		return "", "", ErrInvalid{Msg: fmt.Sprintf("unknown server type %q; expected one of %s",
			typ, strings.Join(ServerTypes(), ", "))}
	}
	meshIP, err := allocateMeshIP(ctx, tx, orgID)
	if err != nil {
		return "", "", err
	}
	id := newID("srv")
	if _, err := tx.Exec(ctx, `
		INSERT INTO servers (id, org_id, name, name_auto, type, source, proxy_role, provider, region, mesh_ip, distro, endpoint, status)
		VALUES ($1, $2, $3, $4, $5, 'provisioned', $6, $7, $8, $9, NULLIF($10, ''), NULLIF($11, ''), 'provisioning')`,
		id, orgID, name, nameAuto, typ, in.ProxyRole, in.Provider, in.Region, meshIP, in.Distro, strings.TrimSpace(in.HostIP)); err != nil {
		return "", "", fmt.Errorf("pre-create server: %w", err)
	}
	return id, name, nil
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

	serverID, name, err := precreateServerTx(ctx, tx, orgID, ProvisionInput{
		Name: serverName, Type: serverType, Provider: provider, Region: region})
	if err != nil {
		return "", "", time.Time{}, err
	}
	token, err = s.issueTokenTx(ctx, tx, orgID, serverID, name, createdBy, expiresAt)
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
	if in.Distro != "" && !DistroSupported(in.Distro) {
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

	serverID, name, err := precreateServerTx(ctx, tx, orgID, in)
	if err != nil {
		return ProvisionResult{}, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE servers SET bootstrap_pubkey = $1, bootstrap_key_wrapped = $2 WHERE id = $3`,
		authorizedKey, wrapped, serverID); err != nil {
		return ProvisionResult{}, fmt.Errorf("store bootstrap key: %w", err)
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

// ReissueBootstrapToken mints a fresh bootstrap keypair + single-use token for
// an EXISTING pre-created server that never finished onboarding (a lost or
// expired install command). Only `provisioning` servers qualify: a registered
// host already holds an agent token, and silently re-arming its bootstrap
// path would let a second machine take over the record.
func (s *Store) ReissueBootstrapToken(ctx context.Context, orgID, serverID, createdBy string, ttl time.Duration) (ProvisionResult, error) {
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

	var name, status string
	err = tx.QueryRow(ctx, `
		SELECT name, status FROM servers
		 WHERE id = $1 AND org_id = $2 AND deleted_at IS NULL
		 FOR UPDATE`, serverID, orgID).Scan(&name, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProvisionResult{}, ErrNotFound
	}
	if err != nil {
		return ProvisionResult{}, fmt.Errorf("load server: %w", err)
	}
	if status != "provisioning" {
		return ProvisionResult{}, fmt.Errorf("%w: server %q is %s — a new install command can only be issued while it is still provisioning", ErrConflict, name, status)
	}

	// One active install command at a time: outstanding unredeemed tokens for
	// this server die with the re-issue.
	if _, err := tx.Exec(ctx,
		`DELETE FROM bootstrap_tokens WHERE server_id = $1 AND used_at IS NULL`, serverID); err != nil {
		return ProvisionResult{}, fmt.Errorf("invalidate old tokens: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE servers SET bootstrap_pubkey = $1, bootstrap_key_wrapped = $2 WHERE id = $3`,
		authorizedKey, wrapped, serverID); err != nil {
		return ProvisionResult{}, fmt.Errorf("store bootstrap key: %w", err)
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
	reported := ParseHostFacts(facts)

	// Populate the pre-created row with what the agent reports at first contact.
	// Name/type/provider/region/mesh_ip were set at provision time and are kept.
	//
	// facts is MERGED rather than assigned, on the same terms as the heartbeat
	// (see RecordHeartbeat): the pre-created row starts at '{}' so this is
	// normally equivalent, and using one rule at both entry points means the
	// absent-is-unchanged guarantee cannot hold on one path and not the other.
	//
	// distro from the agent WINS over the provisioned value. The provisioned
	// one came out of a dropdown the operator picked before they had ever
	// logged into the machine; /etc/os-release is what the machine actually is
	// (SIGMA-201). An agent that could not read it reports nothing and the
	// operator's answer stands.
	//
	// The name follows the same principle one step further (SIGMA-202): when
	// the operator did not name the host, the hostname the machine reports is
	// the name, and name_auto is cleared so this happens exactly once — a
	// machine renamed at the OS level later must not silently rename the server
	// out from under whoever has been calling it something else.
	var srv Server
	err = tx.QueryRow(ctx, `
		UPDATE servers s
		   SET agent_version = $3,
		       facts = s.facts || $4::jsonb,
		       pubkey = NULLIF($5, ''),
		       distro = COALESCE(NULLIF($6, ''), s.distro),
		       name = CASE WHEN s.name_auto AND NULLIF($7, '') IS NOT NULL THEN $7 ELSE s.name END,
		       name_auto = CASE WHEN s.name_auto AND NULLIF($7, '') IS NOT NULL THEN FALSE ELSE s.name_auto END
		 WHERE s.id = $1 AND s.org_id = $2 AND s.deleted_at IS NULL
		 RETURNING id, org_id, name, type, source, proxy_role, provider, region, status, agent_version, facts, mesh_ip, pubkey, last_seen_at, created_at`,
		serverID, orgID, agentVersion, facts, pubkey, reported.Distro, reportedHostname(facts),
	).Scan(&srv.ID, &srv.OrgID, &srv.Name, &srv.Type, &srv.Source, &srv.ProxyRole, &srv.Provider, &srv.Region,
		&srv.Status, &srv.AgentVersion, &srv.Facts, &srv.MeshIP, &srv.Pubkey, &srv.LastSeenAt, &srv.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// Server deleted between provision and register.
		return RegisterResult{}, ErrTokenInvalid
	}
	if err != nil {
		return RegisterResult{}, fmt.Errorf("update server: %w", err)
	}

	// The compatibility gate (SIGMA-203). It runs on the MERGED facts returned
	// above, never on the request payload: an agent re-registering a host whose
	// disk was measured on a previous check-in must be judged on everything
	// known about the machine, not on whatever this one payload happened to
	// carry.
	//
	// A refusal here is not an error — registration SUCCEEDS, the agent gets
	// its token and keeps heartbeating. That is deliberate: the host is fine,
	// the type it was filed under is wrong, and an agent that could not
	// register would have no way to report the corrected facts that clear the
	// state. What changes is the status the operator sees and the two exits the
	// dashboard then offers (change the type, or disconnect).
	fails := CheckServerCompatibility(srv.Type, ParseHostFacts(srv.Facts))
	srv.Status = compatibilityStatus(srv.Status, fails, false)
	srv.IncompatibleReasons = append([]FailedRequirement{}, fails...)
	if err := writeCompatibilityTx(ctx, tx, srv.ID, srv.Status, fails); err != nil {
		return RegisterResult{}, err
	}
	if len(fails) > 0 {
		if err := auditTx(ctx, tx, orgID, "sigmad",
			"Server incompatible — "+IncompatibilitySummary(fails), srv.Name); err != nil {
			return RegisterResult{}, err
		}
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
	       s.agent_version, s.facts, s.mesh_ip, s.endpoint, s.pubkey, s.last_seen_at, s.created_at,
	       s.distro, s.mesh_peer_count, s.hardening_score, s.disk_encrypted, s.ssh_locked,
	       COALESCE(h.keep_public_ssh, TRUE), s.incompatible_reasons,
	       s.decommission_started_at, s.decommission_purge_volumes, ` + readinessExpr + ` AS ready
	  FROM servers s
	  LEFT JOIN server_hardening h ON h.server_id = s.id`

func scanServerRow(row pgx.Row, srv *Server) error {
	if err := row.Scan(&srv.ID, &srv.OrgID, &srv.Name, &srv.Type, &srv.Source, &srv.ProxyRole, &srv.Provider, &srv.Region,
		&srv.Status, &srv.AgentVersion, &srv.Facts, &srv.MeshIP, &srv.Endpoint, &srv.Pubkey, &srv.LastSeenAt, &srv.CreatedAt,
		&srv.Distro, &srv.MeshPeerCount, &srv.HardeningScore, &srv.DiskEncrypted, &srv.SSHLocked,
		&srv.KeepPublicSSH, &srv.IncompatibleReasons,
		&srv.DecommissioningSince, &srv.PurgeVolumes, &srv.Ready); err != nil {
		return err
	}
	// The column is NOT NULL '[]', so this only normalizes JSON's `[]` → Go's
	// nil round trip; the API contract is "always an array", which encoding/json
	// gives us for a nil slice only if we say so — see MarshalJSON's null.
	if srv.IncompatibleReasons == nil {
		srv.IncompatibleReasons = []FailedRequirement{}
	}
	return nil
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
