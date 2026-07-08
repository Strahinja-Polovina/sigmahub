// Package client is sigmad's outbound-only HTTP client for the control plane.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
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
}

type RegisterResponse struct {
	ServerID   string `json:"serverId"`
	AgentToken string `json:"agentToken"`
}

type MetricSample struct {
	CPUPct  float64 `json:"cpuPct"`
	MemPct  float64 `json:"memPct"`
	DiskPct float64 `json:"diskPct"`
	Load1   float64 `json:"load1"`
}

type HeartbeatRequest struct {
	AgentVersion string          `json:"agentVersion"`
	Facts        json.RawMessage `json:"facts"`
	Metrics      *MetricSample   `json:"metrics,omitempty"`
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

func (c *Client) post(ctx context.Context, path, bearer string, body, out any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+path, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
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
