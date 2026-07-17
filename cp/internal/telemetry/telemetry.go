// Package telemetry is the CP side of the P1-13 pipeline: it forwards
// agent-shipped metrics to VictoriaMetrics (cluster mode, per-org accountID
// tenants) and container logs to Loki (X-Scope-OrgID tenants), enforces
// per-org ingest caps, and proxies tenant-isolated queries for the embedded
// dashboards. When no sink is configured the pipeline degrades honestly:
// ingest is acknowledged-and-dropped with a reason, queries 501 — the UI
// shows an explicit "no telemetry pipeline" state, never fabricated data.
package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Config points at the sinks. Empty URL = that half of the pipeline is off.
type Config struct {
	// VMWriteURL is the vminsert base (e.g. http://vminsert:8480); the CP
	// writes to /insert/<tenant>/prometheus/api/v1/import/prometheus.
	VMWriteURL string
	// VMReadURL is the vmselect base (e.g. http://vmselect:8481); queries go
	// to /select/<tenant>/prometheus/api/v1/query_range.
	VMReadURL string
	// LokiURL is the Loki base (push + query with X-Scope-OrgID = org id).
	LokiURL string
}

// Per-org ingest caps (per sweep second): the cardinality/volume backstop
// behind the agent-side allowlist + series cap.
const (
	orgSamplesPerSec = 2000
	orgLinesPerSec   = 1000
)

// Forwarder owns the sink clients and per-org rate limiters.
type Forwarder struct {
	cfg  Config
	http *http.Client

	mu      sync.Mutex
	buckets map[string]*bucket
}

// bucket is a coarse token bucket (refilled per second).
type bucket struct {
	samples, lines int
	window         time.Time
}

func New(cfg Config) *Forwarder {
	return &Forwarder{
		cfg:     cfg,
		http:    &http.Client{Timeout: 10 * time.Second},
		buckets: map[string]*bucket{},
	}
}

func (f *Forwarder) MetricsEnabled() bool { return f.cfg.VMWriteURL != "" }
func (f *Forwarder) QueriesEnabled() bool { return f.cfg.VMReadURL != "" }
func (f *Forwarder) LogsEnabled() bool    { return f.cfg.LokiURL != "" }

// Allow charges an org's ingest budget; false = 429 the batch.
func (f *Forwarder) Allow(orgID string, samples, lines int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	b := f.buckets[orgID]
	now := time.Now()
	if b == nil || now.Sub(b.window) >= time.Second {
		b = &bucket{window: now}
		f.buckets[orgID] = b
	}
	if b.samples+samples > orgSamplesPerSec || b.lines+lines > orgLinesPerSec {
		return false
	}
	b.samples += samples
	b.lines += lines
	return true
}

func (f *Forwarder) post(ctx context.Context, url, contentType string, body []byte, headers map[string]string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := f.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("sink %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// WriteMetrics ships Prometheus text-exposition lines into the org's tenant.
func (f *Forwarder) WriteMetrics(ctx context.Context, tenant int, lines []string) error {
	if !f.MetricsEnabled() || len(lines) == 0 {
		return nil
	}
	u := strings.TrimSuffix(f.cfg.VMWriteURL, "/") +
		"/insert/" + strconv.Itoa(tenant) + "/prometheus/api/v1/import/prometheus"
	return f.post(ctx, u, "text/plain", []byte(strings.Join(lines, "\n")+"\n"), nil)
}

// LokiStream is one labeled log stream in Loki's push format.
type LokiStream struct {
	Stream map[string]string `json:"stream"`
	Values [][2]string       `json:"values"` // [ns-timestamp, line]
}

// WriteLogs pushes labeled streams into the org's Loki tenant.
func (f *Forwarder) WriteLogs(ctx context.Context, orgID string, streams []LokiStream) error {
	if !f.LogsEnabled() || len(streams) == 0 {
		return nil
	}
	body, err := json.Marshal(map[string]any{"streams": streams})
	if err != nil {
		return err
	}
	u := strings.TrimSuffix(f.cfg.LokiURL, "/") + "/loki/api/v1/push"
	return f.post(ctx, u, "application/json", body, map[string]string{"X-Scope-OrgID": orgID})
}

// proxyGet forwards a GET and returns the sink's raw JSON (status, body).
func (f *Forwarder) proxyGet(ctx context.Context, url string, headers map[string]string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := f.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, body, err
}

// QueryRange proxies a PromQL range query into the org's tenant.
func (f *Forwarder) QueryRange(ctx context.Context, tenant int, query string, start, end time.Time, step string) (int, []byte, error) {
	q := url.Values{}
	q.Set("query", query)
	q.Set("start", strconv.FormatInt(start.Unix(), 10))
	q.Set("end", strconv.FormatInt(end.Unix(), 10))
	q.Set("step", step)
	u := strings.TrimSuffix(f.cfg.VMReadURL, "/") +
		"/select/" + strconv.Itoa(tenant) + "/prometheus/api/v1/query_range?" + q.Encode()
	return f.proxyGet(ctx, u, nil)
}

// QueryLogs proxies a LogQL range query into the org's Loki tenant. The
// selector is built by the caller (API layer) from allowlisted parameters —
// tenant isolation itself rides the X-Scope-OrgID header regardless.
func (f *Forwarder) QueryLogs(ctx context.Context, orgID, logql string, start, end time.Time, limit int) (int, []byte, error) {
	q := url.Values{}
	q.Set("query", logql)
	q.Set("start", strconv.FormatInt(start.UnixNano(), 10))
	q.Set("end", strconv.FormatInt(end.UnixNano(), 10))
	q.Set("limit", strconv.Itoa(limit))
	q.Set("direction", "backward")
	u := strings.TrimSuffix(f.cfg.LokiURL, "/") + "/loki/api/v1/query_range?" + q.Encode()
	return f.proxyGet(ctx, u, map[string]string{"X-Scope-OrgID": orgID})
}

// EscapeLabelValue renders a label value safe for the Prometheus text format.
func EscapeLabelValue(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(v)
}
