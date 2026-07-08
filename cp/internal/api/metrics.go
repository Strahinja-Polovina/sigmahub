package api

import (
	"net/http"
	"strconv"
	"time"
)

const (
	defaultMetricsWindow = 24 * time.Hour
	maxMetricsWindow     = 7 * 24 * time.Hour
)

// handleGetMetrics returns an org-scoped server's recent samples. Optional
// ?window=<Go duration> (e.g. 1h, 6h) overrides the 24h default, capped at 7d.
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

	points, err := s.store.MetricsSince(r.Context(), orgID, serverID, time.Now().Add(-window))
	if err != nil {
		s.log.Error("get metrics", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"serverId": serverID,
		"windowSeconds": strconv.Itoa(int(window.Seconds())),
		"points":   points,
	})
}
