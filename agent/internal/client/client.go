// Package client is sigmad's outbound-only HTTP client for the control plane.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/agent/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/agent/internal/mesh"
)

// dsdLongPollTimeout bounds a single DSD long-poll. It must exceed the CP's
// long-poll window (25s) so an idle poll returns a clean 204 instead of the
// client's own timeout firing first (SIGMA-145).
const dsdLongPollTimeout = 35 * time.Second

type Client struct {
	endpoint string
	http     *http.Client
	// poll has no global Timeout: the DSD long-poll legitimately blocks up to
	// the CP's window, so its bound comes from a per-request context deadline
	// (dsdLongPollTimeout) rather than http.Client.Timeout, which would abort
	// every idle poll at 10s and wedge the loop in a permanent error+backoff.
	poll *http.Client
}

func New(endpoint string) *Client {
	return &Client{
		endpoint: endpoint,
		http:     &http.Client{Timeout: 10 * time.Second},
		poll:     &http.Client{},
	}
}

// EndpointIsInsecure reports whether the control-plane endpoint is plaintext http
// to a NON-local host. The DSD public key and the agent token are delivered over
// the enrollment connection (trust-on-first-use), so plaintext to a remote CP
// lets a network attacker on that path substitute both from first boot
// (SIGMA-360). http to localhost is fine (dev, or a local sidecar). Callers warn
// on true; a hard refusal (with an explicit opt-out) is a possible follow-up.
func EndpointIsInsecure(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "https" {
		return false
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1", "":
		return false
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && ip.IsLoopback() {
		return false
	}
	return true
}

type RegisterRequest struct {
	BootstrapToken string          `json:"bootstrapToken"`
	Name           string          `json:"name"`
	AgentVersion   string          `json:"agentVersion"`
	Facts          json.RawMessage `json:"facts"`
	Pubkey         string          `json:"pubkey"`
}

type RegisterResponse struct {
	ServerID     string `json:"serverId"`
	AgentToken   string `json:"agentToken"`
	DSDPublicKey string `json:"dsdPublicKey"`
}

type MetricSample struct {
	CPUPct  float64 `json:"cpuPct"`
	MemPct  float64 `json:"memPct"`
	DiskPct float64 `json:"diskPct"`
	Load1   float64 `json:"load1"`
}

// HardeningReport is the agent's self-assessed host hardening posture (P1-5),
// reported over the heartbeat so the dashboard can show a score + disk-encryption
// status. A daily drift re-check keeps it current.
type HardeningReport struct {
	Score         int  `json:"score"`
	DiskEncrypted bool `json:"diskEncrypted"`
	SSHLocked     bool `json:"sshLocked"`
}

type HeartbeatRequest struct {
	AgentVersion string           `json:"agentVersion"`
	Facts        json.RawMessage  `json:"facts"`
	Pubkey       string           `json:"pubkey"`
	Endpoint     string           `json:"endpoint,omitempty"`
	Metrics      *MetricSample    `json:"metrics,omitempty"`
	Hardening    *HardeningReport `json:"hardening,omitempty"`
	// MeshApplied reports the WireGuard peer config is written for the current
	// peer set; MeshPeerCount is how many peers it covers. Drives Ready.
	MeshApplied   bool `json:"meshApplied"`
	MeshPeerCount int  `json:"meshPeerCount"`
}

// MeshSelf is this server's own mesh identity as the control plane sees it.
type MeshSelf struct {
	ServerID string  `json:"serverId"`
	MeshIP   *string `json:"meshIp"`
	Pubkey   *string `json:"pubkey"`
}

type MeshPeersResponse struct {
	Self  MeshSelf    `json:"self"`
	Peers []mesh.Peer `json:"peers"`
}

// apiError distinguishes definitive rejections (bad/used token, revoked
// credential) from transient failures worth retrying.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string { return fmt.Sprintf("control plane: %d %s", e.Status, e.Body) }

// CredentialRevoked reports that the control plane has definitively rejected
// this agent's identity, so retrying with the same credential cannot succeed.
// It is the ONE predicate both agent loops use to decide a failure is terminal
// (SIGMA-340).
//
// It used to be "any 4xx" (named Permanent), which is not the same question at
// all: an operator fronting the control plane with nginx or Cloudflare gets a
// 408 when one 25-second long-poll trips a timeout rule, and a 429 from a rate
// limit burst. Neither says anything about the credential, but both made the
// DSD loop declare the error terminal and return from its goroutine — the host
// then ignored every deploy, secret rotation, firewall change and agent upgrade
// forever, while the heartbeat (a separate goroutine, still succeeding) kept the
// dashboard showing the server healthy. Only 401/403 mean "this credential is
// dead"; everything else is a transient the caller must back off and retry.
func (e *APIError) CredentialRevoked() bool {
	return e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden
}

func (c *Client) Register(ctx context.Context, req RegisterRequest) (RegisterResponse, error) {
	var res RegisterResponse
	err := c.post(ctx, "/v1/agent/register", "", req, &res)
	return res, err
}

func (c *Client) Heartbeat(ctx context.Context, agentToken string, req HeartbeatRequest) error {
	return c.post(ctx, "/v1/agent/heartbeat", agentToken, req, nil)
}

// MeshPeers fetches this agent's mesh identity and same-org peer list, as a
// CONDITIONAL request (SIGMA-323).
//
// This runs after every successful heartbeat — every 30 seconds — and the
// answer only changes when a server joins the org, re-keys, changes endpoint or
// is removed. Sending the previous ETag lets the control plane answer 304 with
// an empty body, which is what stops N agents each pulling N peer rows every
// half minute: in a 500-server org the unconditional version was serialising
// tens of megabytes of unchanged JSON every 30 seconds, and every org on a
// shared control plane paid it simultaneously.
//
// notModified=true means "reuse what you already have"; res is then zero and
// the returned etag is the one still in force. Pass etag="" to fetch
// unconditionally, which is also what happens on the first poll after a restart.
func (c *Client) MeshPeers(ctx context.Context, agentToken, etag string) (res MeshPeersResponse, newETag string, notModified bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+"/v1/agent/mesh/peers", nil)
	if err != nil {
		return MeshPeersResponse{}, "", false, err
	}
	req.Header.Set("Authorization", "Bearer "+agentToken)
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return MeshPeersResponse{}, "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		// A 304 carries the validator; keep it so the next poll stays
		// conditional. An older control plane that omits it simply makes the
		// next poll unconditional, which is the pre-SIGMA-323 behaviour.
		served := resp.Header.Get("ETag")
		if served == "" {
			served = etag
		}
		return MeshPeersResponse{}, served, true, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return MeshPeersResponse{}, "", false, &APIError{Status: resp.StatusCode, Body: string(bytes.TrimSpace(b))}
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return MeshPeersResponse{}, "", false, err
	}
	// A control plane with no ETag support returns "", which keeps every poll
	// unconditional rather than breaking them.
	return res, resp.Header.Get("ETag"), false, nil
}

// GetDSD long-polls for a DSD newer than `after`. The bool is false when the
// server returned 204 (no newer DSD within the poll window) — the caller
// simply polls again.
func (c *Client) GetDSD(ctx context.Context, agentToken string, after int64) (dsd.Signed, bool, error) {
	// Bound the poll with a context deadline just above the CP window, and issue
	// it on the no-global-timeout `poll` client so an idle long-poll can run the
	// full window and return 204 cleanly.
	ctx, cancel := context.WithTimeout(ctx, dsdLongPollTimeout)
	defer cancel()
	path := fmt.Sprintf("/v1/agent/dsd?after=%d", after)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+path, nil)
	if err != nil {
		return dsd.Signed{}, false, err
	}
	req.Header.Set("Authorization", "Bearer "+agentToken)
	resp, err := c.poll.Do(req)
	if err != nil {
		return dsd.Signed{}, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return dsd.Signed{}, false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return dsd.Signed{}, false, &APIError{Status: resp.StatusCode, Body: string(bytes.TrimSpace(b))}
	}
	var signed dsd.Signed
	if err := json.NewDecoder(resp.Body).Decode(&signed); err != nil {
		return dsd.Signed{}, false, err
	}
	return signed, true, nil
}

// SecretsResponse is the CP's resolved secrets for a resource (values over the
// already-authenticated agent channel).
type SecretsResponse struct {
	Secrets []struct {
		Name   string `json:"name"`
		Value  string `json:"value"`
		EnvVar bool   `json:"envVar"`
	} `json:"secrets"`
}

// FetchSecrets resolves a resource's secrets at container-create time. Org/env
// scope is derived server-side from the agent token, never sent by the caller.
func (c *Client) FetchSecrets(ctx context.Context, agentToken, resourceID string) (SecretsResponse, error) {
	var res SecretsResponse
	err := c.do(ctx, http.MethodGet, "/v1/agent/secrets?resourceId="+url.QueryEscape(resourceID), agentToken, nil, &res)
	return res, err
}

// PostDSDStatus reports per-op results for an applied DSD version.
func (c *Client) PostDSDStatus(ctx context.Context, agentToken string, version int64, ops map[string]json.RawMessage) error {
	return c.post(ctx, "/v1/agent/dsd/status", agentToken, map[string]any{
		"version": version,
		"ops":     ops,
	}, nil)
}

// PostDomainStatus reports the ACME certificate state (P1-8) the agent read from
// Traefik's store. domains is any JSON-marshalable slice of {domain,status,
// serial,expiresAt,error} entries.
func (c *Client) PostDomainStatus(ctx context.Context, agentToken string, domains any) error {
	return c.post(ctx, "/v1/agent/domains/status", agentToken, map[string]any{"domains": domains}, nil)
}

// CloneCredentialResponse is the short-lived clone credential the CP mints for a
// deployment (P1-9). The token is for in-memory use by git.clone only.
type CloneCredentialResponse struct {
	Token        string `json:"token"`
	RepoFullName string `json:"repoFullName"`
	Provider     string `json:"provider"`
}

// FetchCloneCredential resolves the clone credential for a deployment. Scope is
// derived server-side from the agent token (an agent can only fetch a credential
// for a deployment targeting its own host).
func (c *Client) FetchCloneCredential(ctx context.Context, agentToken, deploymentID string) (CloneCredentialResponse, error) {
	var res CloneCredentialResponse
	err := c.do(ctx, http.MethodGet, "/v1/agent/git-credential?deploymentId="+url.QueryEscape(deploymentID), agentToken, nil, &res)
	return res, err
}

// PostBuildLog streams build/orchestration log lines for a deployment (P1-9).
func (c *Client) PostBuildLog(ctx context.Context, agentToken, deploymentID, stream string, lines []string) error {
	return c.post(ctx, "/v1/agent/build-logs", agentToken, map[string]any{
		"deploymentId": deploymentID,
		"stream":       stream,
		"lines":        lines,
	}, nil)
}

// BackupCredentialResponse is the per-run restic material the CP releases to
// this server (P1-11): repo location + key and the S3 credentials. In-memory
// use only; the CP audits every fetch.
type BackupCredentialResponse struct {
	Repository     string `json:"repository"`
	RepoKey        string `json:"repoKey"`
	AccessKey      string `json:"accessKey"`
	SecretKey      string `json:"secretKey"`
	Region         string `json:"region"`
	ForcePathStyle bool   `json:"forcePathStyle"`
}

// FetchBackupCredential resolves one open backup run's repo key + target
// credentials. Scope is derived server-side from the agent token.
func (c *Client) FetchBackupCredential(ctx context.Context, agentToken, runID string) (BackupCredentialResponse, error) {
	var res BackupCredentialResponse
	err := c.do(ctx, http.MethodGet, "/v1/agent/backup-credential?runId="+url.QueryEscape(runID), agentToken, nil, &res)
	return res, err
}

// PostBackupResult reports a backup/verify/restore run's terminal outcome with
// its metadata (snapshot id, dump sha256).
func (c *Client) PostBackupResult(ctx context.Context, agentToken, runID string, ok bool, snapshotID, dumpSha, detail string) error {
	return c.post(ctx, "/v1/agent/backup-status", agentToken, map[string]any{
		"runId":      runID,
		"ok":         ok,
		"snapshotId": snapshotID,
		"dumpSha256": dumpSha,
		"detail":     detail,
	}, nil)
}

// S3OpCredentialResponse is the per-op material for an s3.configure op (P2-1b):
// the root credential to authenticate against the resource's S3 container, plus
// (for create-key) the new per-bucket secret to provision. In-memory only; the
// CP audits every fetch.
type S3OpCredentialResponse struct {
	RootAccessKey string `json:"rootAccessKey"`
	RootSecretKey string `json:"rootSecretKey"`
	NewSecretKey  string `json:"newSecretKey"`
}

// FetchS3OpCredential resolves one open s3.configure op's credential. Scope is
// derived server-side from the agent token.
func (c *Client) FetchS3OpCredential(ctx context.Context, agentToken, opID string) (S3OpCredentialResponse, error) {
	var res S3OpCredentialResponse
	err := c.do(ctx, http.MethodGet, "/v1/agent/s3-op-credential?opId="+url.QueryEscape(opID), agentToken, nil, &res)
	return res, err
}

// PostS3OpStatus reports an s3.configure op's terminal outcome (measuredBytes is
// set only for measure ops, feeding storage metering).
func (c *Client) PostS3OpStatus(ctx context.Context, agentToken, opID string, ok bool, detail string, measuredBytes int64) error {
	return c.post(ctx, "/v1/agent/s3-op-status", agentToken, map[string]any{
		"opId":          opID,
		"ok":            ok,
		"detail":        detail,
		"measuredBytes": measuredBytes,
	}, nil)
}

// ClusterNodeStatus is this node's own account of k3s on it. Without it a
// cluster has no way to leave 'provisioning': the control plane can see that
// the agent is checking in, which says nothing about whether k3s came up.
type ClusterNodeStatus struct {
	Ready       bool   `json:"ready"`
	Message     string `json:"message,omitempty"`
	APIEndpoint string `json:"apiEndpoint,omitempty"`
	Version     string `json:"version,omitempty"`
}

// PostClusterStatus reports this node's k3s state. Scope is derived server-side
// from the agent token — a node can only ever report about itself.
func (c *Client) PostClusterStatus(ctx context.Context, agentToken string, st ClusterNodeStatus) error {
	return c.post(ctx, "/v1/agent/cluster-status", agentToken, st, nil)
}

// PostUninstallAck is the final word of a graceful decommission (SIGMA-204):
// the workloads are gone and this agent is about to remove itself. The control
// plane answers by tombstoning the server and revoking this very token, so
// nothing may be sent on this channel afterwards — and, critically, nothing the
// uninstall handler does before this call may break the channel. See
// internal/uninstall for the ordering.
//
// ok=false still completes the decommission; detail says what did not tear
// down, so the operator is told to finish by hand instead of being left with a
// row that looks clean.
func (c *Client) PostUninstallAck(ctx context.Context, agentToken string, ok bool, detail string) error {
	return c.post(ctx, "/v1/agent/uninstall-ack", agentToken,
		map[string]any{"ok": ok, "detail": detail}, nil)
}

// RegistryCredentialResponse authenticates a push (on a build server) or a pull
// (on a cluster node) against the org's registry. In-memory only; the CP audits
// every release.
type RegistryCredentialResponse struct {
	Host     string `json:"host"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// FetchRegistryCredential resolves the org's registry credential.
func (c *Client) FetchRegistryCredential(ctx context.Context, agentToken string) (RegistryCredentialResponse, error) {
	var res RegistryCredentialResponse
	err := c.do(ctx, http.MethodGet, "/v1/agent/registry-credential", agentToken, nil, &res)
	return res, err
}

// WALTarget is a PITR-enabled resource whose WAL this server ships (P2-5).
type WALTarget struct {
	ResourceID string `json:"resourceId"`
}

// FetchWALTargets lists the resources whose spool the agent should drain.
func (c *Client) FetchWALTargets(ctx context.Context, agentToken string) ([]WALTarget, error) {
	var res struct {
		Targets []WALTarget `json:"targets"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/agent/wal-targets", agentToken, nil, &res)
	return res.Targets, err
}

// FetchWALCredential resolves the restic credential for a resource's WAL
// shipping (audited per release; the caller caches it).
func (c *Client) FetchWALCredential(ctx context.Context, agentToken, resourceID string) (BackupCredentialResponse, error) {
	var res BackupCredentialResponse
	err := c.do(ctx, http.MethodGet, "/v1/agent/wal-credential?resourceId="+url.QueryEscape(resourceID), agentToken, nil, &res)
	return res, err
}

// PostWALStatus records a shipping cycle's high-water mark.
func (c *Client) PostWALStatus(ctx context.Context, agentToken, resourceID, lastSegment string) error {
	return c.post(ctx, "/v1/agent/wal-status", agentToken, map[string]any{
		"resourceId":  resourceID,
		"lastSegment": lastSegment,
	}, nil)
}

// TelemetrySample is one metric point shipped over the outbound channel
// (P1-13). Labels are restricted to the agent-suppliable allowlist
// {resource, service}; the CP adds {org, project, env, server} itself.
type TelemetrySample struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
	Value  float64           `json:"value"`
	TS     int64             `json:"ts"` // unix millis
}

// TelemetryAck is the CP's ingest answer; Accepted=false carries the reason
// (e.g. no sink configured) so the agent backs off instead of retrying hot.
type TelemetryAck struct {
	Accepted bool   `json:"accepted"`
	Reason   string `json:"reason"`
}

// PostTelemetryMetrics ships one metric batch. dropped counts series the
// agent-side cap discarded (surfaced in CP logs for cardinality forensics).
func (c *Client) PostTelemetryMetrics(ctx context.Context, agentToken string, samples []TelemetrySample, dropped int) (TelemetryAck, error) {
	var ack TelemetryAck
	err := c.post(ctx, "/v1/agent/telemetry/metrics", agentToken, map[string]any{
		"samples": samples,
		"dropped": dropped,
	}, &ack)
	return ack, err
}

// TelemetryLogLine / TelemetryLogStream carry container stdout/stderr batches.
type TelemetryLogLine struct {
	TS   int64  `json:"ts"` // unix millis
	Text string `json:"text"`
}

type TelemetryLogStream struct {
	ResourceID string             `json:"resourceId"`
	Service    string             `json:"service,omitempty"`
	Stream     string             `json:"stream"`
	Lines      []TelemetryLogLine `json:"lines"`
}

// PostTelemetryLogs ships one log batch. dropped counts lines the bounded
// agent buffer discarded under backpressure.
func (c *Client) PostTelemetryLogs(ctx context.Context, agentToken string, streams []TelemetryLogStream, dropped int) (TelemetryAck, error) {
	var ack TelemetryAck
	err := c.post(ctx, "/v1/agent/telemetry/logs", agentToken, map[string]any{
		"streams": streams,
		"dropped": dropped,
	}, &ack)
	return ack, err
}

func (c *Client) post(ctx context.Context, path, bearer string, body, out any) error {
	return c.do(ctx, http.MethodPost, path, bearer, body, out)
}

func (c *Client) do(ctx context.Context, method, path, bearer string, body, out any) error {
	var payload io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, payload)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &APIError{Status: resp.StatusCode, Body: string(bytes.TrimSpace(b))}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
