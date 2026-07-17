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
			meta, ok := metaCache[resID]
			if !ok {
				m, err := s.tel.TelemetryResourceMetaForServer(r.Context(), srv.ID, resID)
				if errors.Is(err, store.ErrNotFound) {
					metaCache[resID] = nil
					continue // foreign/unknown resource label: dropped
				} else if err != nil {
					s.log.Error("telemetry meta", "err", err)
					continue
				}
				meta = &m
				metaCache[resID] = meta
			}
			if meta == nil {
				continue
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
		meta, ok := metaCache[st.ResourceID]
		if !ok {
			m, err := s.tel.TelemetryResourceMetaForServer(r.Context(), srv.ID, st.ResourceID)
			if errors.Is(err, store.ErrNotFound) {
				metaCache[st.ResourceID] = nil
				continue
			} else if err != nil {
				s.log.Error("telemetry meta", "err", err)
				continue
			}
			meta = &m
			metaCache[st.ResourceID] = meta
		}
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
