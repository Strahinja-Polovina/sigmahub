package api

import (
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

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
