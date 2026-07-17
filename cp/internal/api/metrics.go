package api

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

const (
	defaultMetricsWindow = 24 * time.Hour
	maxMetricsWindow     = 7 * 24 * time.Hour
)

// handleGetMetrics returns an org-scoped server's recent samples. Optional
// ?window=<Go duration> (e.g. 1h, 6h) overrides the 24h default, capped at 7d.
//
// P1-13 cutover: when the telemetry pipeline is configured this endpoint is
// re-pointed at VictoriaMetrics (the sigmahub_host_* series), falling back to
// the interim server_metrics store on any pipeline miss — the heartbeat
// gauges REMAIN either way (the staleness sweeper depends on them).
func (s *Server) handleGetMetrics(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("orgId")
	serverID := r.PathValue("serverId")

	window := defaultMetricsWindow
	if q := r.URL.Query().Get("window"); q != "" {
		if d, err := time.ParseDuration(q); err == nil && d > 0 {
			window = d
		}
	}
	if window > maxMetricsWindow {
		window = maxMetricsWindow
	}

	if points, ok := s.hostMetricsFromPipeline(r, orgID, serverID, window); ok {
		writeJSON(w, http.StatusOK, map[string]any{
			"serverId":      serverID,
			"windowSeconds": strconv.Itoa(int(window.Seconds())),
			"points":        points,
			"source":        "pipeline",
		})
		return
	}

	points, err := s.store.MetricsSince(r.Context(), orgID, serverID, time.Now().Add(-window))
	if err != nil {
		s.log.Error("get metrics", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"serverId":      serverID,
		"windowSeconds": strconv.Itoa(int(window.Seconds())),
		"points":        points,
	})
}

// hostMetricsFromPipeline queries the sigmahub_host_* series for one server
// and maps the matrix result onto the legacy MetricPoint shape, so the web
// chart is transparently re-pointed. ok=false on any miss → caller falls back.
func (s *Server) hostMetricsFromPipeline(r *http.Request, orgID, serverID string, window time.Duration) ([]store.MetricPoint, bool) {
	if s.telemetry == nil || s.tel == nil || !s.telemetry.QueriesEnabled() {
		return nil, false
	}
	tenant, err := s.tel.OrgTenant(r.Context(), orgID)
	if err != nil {
		return nil, false
	}
	end := time.Now()
	start := end.Add(-window)
	query := `{__name__=~"sigmahub_host_cpu_pct|sigmahub_host_mem_pct|sigmahub_host_disk_pct|sigmahub_host_load1",server="` + serverID + `"}`
	status, body, err := s.telemetry.QueryRange(r.Context(), tenant, query, start, end, "60")
	if err != nil || status != http.StatusOK {
		return nil, false
	}
	var res struct {
		Data struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Values [][2]any          `json:"values"` // [unix, "value"]
			} `json:"result"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &res) != nil || len(res.Data.Result) == 0 {
		return nil, false
	}
	byTS := map[int64]*store.MetricPoint{}
	for _, series := range res.Data.Result {
		name := series.Metric["__name__"]
		for _, v := range series.Values {
			tsF, ok := v[0].(float64)
			if !ok {
				continue
			}
			valS, ok := v[1].(string)
			if !ok {
				continue
			}
			val, err := strconv.ParseFloat(valS, 64)
			if err != nil {
				continue
			}
			ts := int64(tsF)
			p := byTS[ts]
			if p == nil {
				p = &store.MetricPoint{RecordedAt: time.Unix(ts, 0).UTC()}
				byTS[ts] = p
			}
			switch name {
			case "sigmahub_host_cpu_pct":
				p.CPUPct = val
			case "sigmahub_host_mem_pct":
				p.MemPct = val
			case "sigmahub_host_disk_pct":
				p.DiskPct = val
			case "sigmahub_host_load1":
				p.Load1 = val
			}
		}
	}
	points := make([]store.MetricPoint, 0, len(byTS))
	for _, p := range byTS {
		points = append(points, *p)
	}
	sort.Slice(points, func(i, j int) bool { return points[i].RecordedAt.Before(points[j].RecordedAt) })
	return points, true
}
