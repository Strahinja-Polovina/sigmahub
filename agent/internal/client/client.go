// Package client is sigmad's outbound-only HTTP client for the control plane.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/agent/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/agent/internal/mesh"
)

type Client struct {
	endpoint string
	http     *http.Client
}

func New(endpoint string) *Client {
	return &Client{
		endpoint: endpoint,
		http:     &http.Client{Timeout: 10 * time.Second},
	}
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

// apiError distinguishes definitive rejections (4xx: bad/used token, revoked
// credential) from transient failures worth retrying.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string { return fmt.Sprintf("control plane: %d %s", e.Status, e.Body) }

func (e *APIError) Permanent() bool { return e.Status >= 400 && e.Status < 500 }

func (c *Client) Register(ctx context.Context, req RegisterRequest) (RegisterResponse, error) {
	var res RegisterResponse
	err := c.post(ctx, "/v1/agent/register", "", req, &res)
	return res, err
}

func (c *Client) Heartbeat(ctx context.Context, agentToken string, req HeartbeatRequest) error {
	return c.post(ctx, "/v1/agent/heartbeat", agentToken, req, nil)
}

// MeshPeers fetches this agent's mesh identity and same-org peer list.
func (c *Client) MeshPeers(ctx context.Context, agentToken string) (MeshPeersResponse, error) {
	var res MeshPeersResponse
	err := c.do(ctx, http.MethodGet, "/v1/agent/mesh/peers", agentToken, nil, &res)
	return res, err
}

// GetDSD long-polls for a DSD newer than `after`. The bool is false when the
// server returned 204 (no newer DSD within the poll window) — the caller
// simply polls again.
func (c *Client) GetDSD(ctx context.Context, agentToken string, after int64) (dsd.Signed, bool, error) {
	path := fmt.Sprintf("/v1/agent/dsd?after=%d", after)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+path, nil)
	if err != nil {
		return dsd.Signed{}, false, err
	}
	req.Header.Set("Authorization", "Bearer "+agentToken)
	resp, err := c.http.Do(req)
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
