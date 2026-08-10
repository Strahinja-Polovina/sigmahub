package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/cpmetrics"
)

// SIGMA-248: the control plane runs six unsupervised background loops and, from
// outside, "the process is up" and "the loops are working" were the same
// observation. This asserts the surface that tells them apart: a per-loop
// last-success timestamp on /metrics.
func TestMetricsEndpointReportsLoopHeartbeats(t *testing.T) {
	s := New(slog.Default(), fakePinger{}, &fakeStore{}, &fakeDomain{}, Options{
		DevServiceToken: testServiceToken,
	})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("GET /metrics = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `sigmahub_cp_loop_last_success_seconds{loop="backup_scheduler"}`) {
		t.Fatalf("/metrics does not report the backup scheduler's last success:\n%s", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type = %q, want the Prometheus text exposition type", ct)
	}

	// And a supplied registry's heartbeats reach the endpoint: the loop the
	// scheduler reports through is the loop a scrape reads.
	reg := cpmetrics.New()
	reg.Loop(cpmetrics.LoopBackupScheduler).Report(nil)
	s = New(slog.Default(), fakePinger{}, &fakeStore{}, &fakeDomain{}, Options{
		DevServiceToken: testServiceToken,
		Metrics:         reg,
	})
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if strings.Contains(rec.Body.String(), `sigmahub_cp_loop_last_success_seconds{loop="backup_scheduler"} 0.000`) {
		t.Fatalf("a reported pass did not reach /metrics:\n%s", rec.Body.String())
	}
}

// TestGetMetrics_FallbackClampsWindowToRetention pins SIGMA-257: with no
// telemetry pipeline configured the handler answers from server_metrics, which
// the sweeper prunes at the configured retention. Echoing back a seven-day
// window the store cannot answer makes an empty chart axis read as "the host
// was down for six days" — so the served window must be the retention, and the
// response must say it was clamped and why.
func TestGetMetrics_FallbackClampsWindowToRetention(t *testing.T) {
	s := newTestServer(t, nil)
	req := httptest.NewRequest("GET", "/v1/orgs/org_1/servers/srv_1/metrics?window=168h", nil)
	req.Header.Set("Authorization", "Bearer "+testServiceToken)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	var res struct {
		WindowSeconds string `json:"windowSeconds"`
		ClampedTo     string `json:"clampedTo"`
		Reason        string `json:"reason"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.WindowSeconds != "86400" {
		t.Fatalf("windowSeconds = %q, want %q (24h retention); body %s",
			res.WindowSeconds, "86400", rec.Body)
	}
	if res.ClampedTo != "86400" || res.Reason == "" {
		t.Fatalf("want an explicit clamp reason, got clampedTo=%q reason=%q; body %s",
			res.ClampedTo, res.Reason, rec.Body)
	}
}

// TestGetMetrics_FallbackHonoursConfiguredRetention pins the wiring: an
// operator who lengthens CP_METRICS_RETENTION (the same value the sweeper
// prunes at) gets the longer window served, with no clamp. Without the Options
// field this would silently keep clamping at the built-in 24h, which is the
// two-places-for-one-fact bug SIGMA-257 was about.
func TestGetMetrics_FallbackHonoursConfiguredRetention(t *testing.T) {
	s := New(slog.Default(), fakePinger{}, &fakeStore{}, &fakeDomain{}, Options{
		DevServiceToken:  testServiceToken,
		MetricsRetention: 7 * 24 * time.Hour,
	})
	req := httptest.NewRequest("GET", "/v1/orgs/org_1/servers/srv_1/metrics?window=168h", nil)
	req.Header.Set("Authorization", "Bearer "+testServiceToken)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	var res struct {
		WindowSeconds string `json:"windowSeconds"`
		ClampedTo     string `json:"clampedTo"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.WindowSeconds != "604800" || res.ClampedTo != "" {
		t.Fatalf("windowSeconds = %q clampedTo = %q, want 604800 unclamped; body %s",
			res.WindowSeconds, res.ClampedTo, rec.Body)
	}
}

// TestGetMetrics_FallbackKeepsWindowWithinRetention is the other half: a window
// the store CAN answer is served verbatim, with no clamp annotation.
func TestGetMetrics_FallbackKeepsWindowWithinRetention(t *testing.T) {
	s := newTestServer(t, nil)
	req := httptest.NewRequest("GET", "/v1/orgs/org_1/servers/srv_1/metrics?window=6h", nil)
	req.Header.Set("Authorization", "Bearer "+testServiceToken)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	var res struct {
		WindowSeconds string `json:"windowSeconds"`
		ClampedTo     string `json:"clampedTo"`
		Reason        string `json:"reason"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.WindowSeconds != "21600" {
		t.Fatalf("windowSeconds = %q, want 21600", res.WindowSeconds)
	}
	if res.ClampedTo != "" || res.Reason != "" {
		t.Fatalf("unclamped window must not be annotated: clampedTo=%q reason=%q", res.ClampedTo, res.Reason)
	}
}
