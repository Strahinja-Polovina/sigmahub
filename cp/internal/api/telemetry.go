package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/telemetry"
)

// TelemetryAPI is the store slice the telemetry endpoints need.
type TelemetryAPI interface {
	OrgTenant(ctx context.Context, orgID string) (int, error)
	TelemetryResourceMetaForServer(ctx context.Context, serverID, resourceID string) (store.TelemetryResourceMeta, error)
	DeployStatsForOrg(ctx context.Context, orgID string, window int) (store.DeployStats, error)
	ConnectedServerCount(ctx context.Context, orgID string) (int, error)
	VerifyDays(ctx context.Context, orgID string, days int) ([]store.VerifyDay, error)
}

// telemetryMetaTTL is how long a resolved (project, environment) label pair is
// reused across ingest batches (SIGMA-333).
//
// A minute is far longer than the ingest cadence — the agent's metrics shipper
// pushes every 15s and its log flusher every 3s — and far shorter than anything
// that can change the answer. In practice the answer cannot change at all: a
// resource's project, environment and server are written once at creation and
// no code path updates them, so the only event this TTL exists for is deletion,
// where a minute of labels on a resource that has just gone away is harmless.
// That is also why there is no explicit invalidation hook: there is no mutation
// to hang one on.
const telemetryMetaTTL = time.Minute

// telemetryMetaCache memoises resource label lookups ACROSS requests, keyed by
// (serverID, resourceID) — the same scoping the query uses, so the tenant
// isolation the lookup provides is preserved by the cache key rather than
// re-checked.
//
// Both ingest handlers used to memoise only within a single request, so every
// batch re-queried the resources table once per distinct resource it mentioned.
// The labels are static, the batches are not: 500 servers hosting 10 resources
// each meant 5,000 lookups every 15 seconds from the metrics endpoint alone,
// about 330 queries a second, plus the log endpoint at a 3-second cadence. All
// of it competed for the same 20-connection pool as the work that actually has
// a deadline — DSD renders, long-poll wake-ups, the backup scheduler's minute
// tick — so telemetry ingest was a database floor that rose linearly with fleet
// size and pushed out everything else, with no obvious cause from outside.
//
// Only POSITIVE results are cached. A resource that is unknown to this server
// is re-queried every batch, deliberately: caching the "no" would make a
// resource created moments before a batch unlabelable, and therefore invisible
// in the dashboard, for the whole TTL.
//
// A nil *telemetryMetaCache is usable and never caches, so a zero-value Server
// needs no branch.
type telemetryMetaCache struct {
	mu        sync.Mutex
	entries   map[string]telemetryMetaEntry
	lastSweep time.Time
}

type telemetryMetaEntry struct {
	meta    store.TelemetryResourceMeta
	expires time.Time
}

func newTelemetryMetaCache() *telemetryMetaCache {
	return &telemetryMetaCache{entries: map[string]telemetryMetaEntry{}, lastSweep: time.Now()}
}

// telemetryMetaKey uses a NUL separator so no server/resource id pair can be
// spelled two ways.
func telemetryMetaKey(serverID, resourceID string) string {
	return serverID + "\x00" + resourceID
}

func (c *telemetryMetaCache) get(serverID, resourceID string) (store.TelemetryResourceMeta, bool) {
	if c == nil {
		return store.TelemetryResourceMeta{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[telemetryMetaKey(serverID, resourceID)]
	if !ok || time.Now().After(e.expires) {
		return store.TelemetryResourceMeta{}, false
	}
	return e.meta, true
}

func (c *telemetryMetaCache) put(serverID, resourceID string, meta store.TelemetryResourceMeta) {
	if c == nil {
		return
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]telemetryMetaEntry{}
	}
	// Sweep expired entries at most once per TTL. Without it the map would keep
	// a row for every resource ever deleted from the fleet, which is a slow leak
	// in the one process that is meant to stay up for months.
	if now.Sub(c.lastSweep) > telemetryMetaTTL {
		for k, e := range c.entries {
			if now.After(e.expires) {
				delete(c.entries, k)
			}
		}
		c.lastSweep = now
	}
	c.entries[telemetryMetaKey(serverID, resourceID)] = telemetryMetaEntry{
		meta: meta, expires: now.Add(telemetryMetaTTL),
	}
}

// telemetryResourceMeta resolves a resource's project/env labels through two
// tiers: the caller's per-request map (so one batch does at most one lookup per
// id, which is what the handlers already guaranteed) and the process-level TTL
// cache above. It returns nil when the resource is not this server's — the
// caller drops the sample or stream.
func (s *Server) telemetryResourceMeta(ctx context.Context, serverID, resourceID string, perRequest map[string]*store.TelemetryResourceMeta) *store.TelemetryResourceMeta {
	if meta, ok := perRequest[resourceID]; ok {
		return meta
	}
	if cached, ok := s.telMeta.get(serverID, resourceID); ok {
		meta := &cached
		perRequest[resourceID] = meta
		return meta
	}
	m, err := s.tel.TelemetryResourceMetaForServer(ctx, serverID, resourceID)
	if errors.Is(err, store.ErrNotFound) {
		perRequest[resourceID] = nil // not cached process-wide; see above
		return nil
	}
	if err != nil {
		// A transient failure is not an answer: record nothing in either tier so
		// the next batch retries rather than dropping this resource's telemetry
		// for a minute.
		s.log.Error("telemetry meta", "err", err)
		return nil
	}
	s.telMeta.put(serverID, resourceID, m)
	meta := &m
	perRequest[resourceID] = meta
	return meta
}

// metricNamePattern gates agent-shipped metric names to the sigmahub_
// namespace with ordinary Prometheus name characters.
var metricNamePattern = regexp.MustCompile(`^sigmahub_[a-zA-Z0-9_]{1,120}$`)

// allowedSampleLabels is the agent-suppliable half of the hard label
// allowlist; the CP adds {org, project, env, server} itself. Anything else is
// dropped pre-forward (the cardinality contract).
var allowedSampleLabels = map[string]bool{"resource": true, "service": true, "stream": true}

type telemetrySample struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels"`
	Value  float64           `json:"value"`
	TS     int64             `json:"ts"` // unix millis
}

// handleAgentTelemetryMetrics ingests one agent's 15s metric batch, enforces
// the label allowlist + per-org caps, enriches with tenancy labels and
// forwards to VictoriaMetrics. With no sink configured the batch is
// acknowledged-and-dropped with an explicit reason (never silently "stored").
func (s *Server) handleAgentTelemetryMetrics(w http.ResponseWriter, r *http.Request) {
	srv := serverFrom(r)
	var req struct {
		Samples []telemetrySample `json:"samples"`
		Dropped int               `json:"dropped"` // agent-side cap overflow, logged for ops
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if s.telemetry == nil || s.tel == nil || !s.telemetry.MetricsEnabled() {
		writeJSON(w, http.StatusOK, map[string]any{"accepted": false, "reason": "no metrics sink configured"})
		return
	}
	if !s.telemetry.Allow(srv.OrgID, len(req.Samples), 0) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "org ingest cap exceeded"})
		return
	}
	if req.Dropped > 0 {
		s.log.Warn("agent dropped series over its cap", "server", srv.ID, "dropped", req.Dropped)
	}

	// Resolve project/env once per distinct resource label (server-scoped).
	metaCache := map[string]*store.TelemetryResourceMeta{}
	lines := make([]string, 0, len(req.Samples))
	for _, smp := range req.Samples {
		if !metricNamePattern.MatchString(smp.Name) {
			continue // unknown/off-namespace metric: dropped pre-egress
		}
		labels := map[string]string{
			"org":    srv.OrgID,
			"server": srv.ID,
		}
		for k, v := range smp.Labels {
			if !allowedSampleLabels[k] || v == "" {
				continue // unknown label: dropped pre-egress (tested contract)
			}
			labels[k] = v
		}
		if resID := labels["resource"]; resID != "" {
			meta := s.telemetryResourceMeta(r.Context(), srv.ID, resID, metaCache)
			if meta == nil {
				continue // foreign/unknown resource label: dropped
			}
			labels["project"] = meta.ProjectID
			labels["env"] = meta.EnvironmentID
		}
		keys := make([]string, 0, len(labels))
		for k := range labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		b.WriteString(smp.Name)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(k)
			b.WriteString(`="`)
			b.WriteString(telemetry.EscapeLabelValue(labels[k]))
			b.WriteByte('"')
		}
		b.WriteString("} ")
		b.WriteString(strconv.FormatFloat(smp.Value, 'g', -1, 64))
		b.WriteByte(' ')
		b.WriteString(strconv.FormatInt(smp.TS, 10))
		lines = append(lines, b.String())
	}

	tenant, err := s.tel.OrgTenant(r.Context(), srv.OrgID)
	if err != nil {
		s.log.Error("org tenant", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if err := s.telemetry.WriteMetrics(r.Context(), tenant, lines); err != nil {
		s.log.Error("metrics forward", "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "metrics sink unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accepted": true, "forwarded": len(lines)})
}

// handleAgentTelemetryLogs ingests container stdout/stderr batches and pushes
// them to Loki under the org tenant with the allowlisted label set.
func (s *Server) handleAgentTelemetryLogs(w http.ResponseWriter, r *http.Request) {
	srv := serverFrom(r)
	var req struct {
		Streams []struct {
			ResourceID string `json:"resourceId"`
			Service    string `json:"service"`
			Stream     string `json:"stream"` // stdout | stderr
			Lines      []struct {
				TS   int64  `json:"ts"` // unix millis
				Text string `json:"text"`
			} `json:"lines"`
		} `json:"streams"`
		Dropped int `json:"dropped"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if s.telemetry == nil || s.tel == nil || !s.telemetry.LogsEnabled() {
		writeJSON(w, http.StatusOK, map[string]any{"accepted": false, "reason": "no logs sink configured"})
		return
	}
	total := 0
	for _, st := range req.Streams {
		total += len(st.Lines)
	}
	if !s.telemetry.Allow(srv.OrgID, 0, total) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "org ingest cap exceeded"})
		return
	}
	if req.Dropped > 0 {
		s.log.Warn("agent dropped log lines over its buffer", "server", srv.ID, "dropped", req.Dropped)
	}

	metaCache := map[string]*store.TelemetryResourceMeta{}
	out := make([]telemetry.LokiStream, 0, len(req.Streams))
	for _, st := range req.Streams {
		if st.ResourceID == "" || len(st.Lines) == 0 {
			continue
		}
		meta := s.telemetryResourceMeta(r.Context(), srv.ID, st.ResourceID, metaCache)
		if meta == nil {
			continue
		}
		stream := map[string]string{
			"org":      srv.OrgID,
			"project":  meta.ProjectID,
			"env":      meta.EnvironmentID,
			"server":   srv.ID,
			"resource": st.ResourceID,
			"stream":   st.Stream,
		}
		if st.Service != "" {
			stream["service"] = st.Service
		}
		values := make([][2]string, 0, len(st.Lines))
		for _, l := range st.Lines {
			values = append(values, [2]string{strconv.FormatInt(l.TS*int64(time.Millisecond), 10), l.Text})
		}
		out = append(out, telemetry.LokiStream{Stream: stream, Values: values})
	}
	if err := s.telemetry.WriteLogs(r.Context(), srv.OrgID, out); err != nil {
		s.log.Error("logs forward", "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "logs sink unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accepted": true, "forwarded": len(out)})
}

// parseRange reads start/end/step query params with sane 24h defaults.
func parseRange(r *http.Request) (start, end time.Time, step string) {
	end = time.Now()
	start = end.Add(-24 * time.Hour)
	if v, err := strconv.ParseInt(r.URL.Query().Get("start"), 10, 64); err == nil && v > 0 {
		start = time.Unix(v, 0)
	}
	if v, err := strconv.ParseInt(r.URL.Query().Get("end"), 10, 64); err == nil && v > 0 {
		end = time.Unix(v, 0)
	}
	step = r.URL.Query().Get("step")
	if step == "" {
		step = "300"
	}
	return start, end, step
}

// handleMetricsQuery proxies a PromQL range query into the org's tenant for
// the embedded dashboards. Tenant isolation is the VictoriaMetrics accountID;
// the query itself cannot cross it.
func (s *Server) handleMetricsQuery(w http.ResponseWriter, r *http.Request) {
	if s.telemetry == nil || s.tel == nil || !s.telemetry.QueriesEnabled() {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "telemetry_not_configured"})
		return
	}
	query := r.URL.Query().Get("query")
	if query == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "query is required"})
		return
	}
	tenant, err := s.tel.OrgTenant(r.Context(), r.PathValue("orgId"))
	if err != nil {
		s.log.Error("org tenant", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	start, end, step := parseRange(r)
	status, body, err := s.telemetry.QueryRange(r.Context(), tenant, query, start, end, step)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "metrics sink unavailable"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// logQLLabelValue guards values interpolated into the server-built selector.
var logQLLabelValue = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

// handleLogsQuery serves the env-wide log search: the LogQL selector is built
// SERVER-SIDE from allowlisted parameters ({org} always; optional resource/
// env/server) plus an optional line-contains filter, so no raw LogQL crosses
// the org API surface.
func (s *Server) handleLogsQuery(w http.ResponseWriter, r *http.Request) {
	if s.telemetry == nil || s.tel == nil || !s.telemetry.LogsEnabled() {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "telemetry_not_configured"})
		return
	}
	orgID := r.PathValue("orgId")
	// The org label decides which tenant's logs come back, and it is the only
	// value in this selector that was interpolated unchecked while every sibling
	// below is validated (SIGMA-349). Nothing can currently reach here with a
	// LogQL metacharacter — requireService refuses a token/path org mismatch and
	// orgIDPattern pins ids to [A-Za-z0-9_-] at provisioning — but that invariant
	// is load-bearing for tenant isolation and lives in another file, on another
	// request. Anything that widens org ids later (a vanity slug, an SSO-derived
	// id, an import that writes org_tenants directly) would turn a naming change
	// into cross-tenant log disclosure with nothing here to catch it.
	if !logQLLabelValue.MatchString(orgID) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid organization id"})
		return
	}
	sel := []string{`org="` + orgID + `"`}
	for _, key := range []string{"resource", "env", "server", "service"} {
		if v := r.URL.Query().Get(key); v != "" {
			if !logQLLabelValue.MatchString(v) {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid " + key + " filter"})
				return
			}
			sel = append(sel, key+`="`+v+`"`)
		}
	}
	logql := "{" + strings.Join(sel, ",") + "}"
	if q := r.URL.Query().Get("q"); q != "" {
		logql += fmt.Sprintf(" |= %q", q)
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	start, end, _ := parseRange(r)
	status, body, err := s.telemetry.QueryLogs(r.Context(), orgID, logql, start, end, limit)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "logs sink unavailable"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// handleBetaMetrics is the M1 exit-criteria feed (org-scoped): deploy success
// rate over the last 500 terminal deploys (immutable P1-9 rows), the first
// successful deploy (TTFD's deploy leg — the signup leg joins web-side), the
// restore-verify green streak computed from P1-11's per-day feed, and the
// connected-server count.
func (s *Server) handleBetaMetrics(w http.ResponseWriter, r *http.Request) {
	if s.tel == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "telemetry_not_configured"})
		return
	}
	orgID := r.PathValue("orgId")
	stats, err := s.tel.DeployStatsForOrg(r.Context(), orgID, 500)
	if err != nil {
		s.writeStoreErr(w, err, "beta metrics: deploys")
		return
	}
	days, err := s.tel.VerifyDays(r.Context(), orgID, 90)
	if err != nil {
		s.writeStoreErr(w, err, "beta metrics: verify days")
		return
	}
	// Streak: consecutive green days ending today (a not-green today breaks it —
	// zero-run days are never green, per the SIGMA-50 predicate).
	streak := 0
	for i := len(days) - 1; i >= 0; i-- {
		if !days[i].Green {
			break
		}
		streak++
	}
	servers, err := s.tel.ConnectedServerCount(r.Context(), orgID)
	if err != nil {
		s.writeStoreErr(w, err, "beta metrics: servers")
		return
	}
	rate := 0.0
	if stats.Total > 0 {
		rate = float64(stats.Succeeded) / float64(stats.Total)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"deploys": map[string]any{
			"window":    stats.Window,
			"total":     stats.Total,
			"succeeded": stats.Succeeded,
			"rate":      rate,
		},
		"firstDeployAt":    stats.FirstDeployAt,
		"verifyStreakDays": streak,
		"connectedServers": servers,
	})
}
