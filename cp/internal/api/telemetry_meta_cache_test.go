package api

// SIGMA-333: telemetry label enrichment must not re-query the resource table on
// every ingest batch.
//
// The agent's metrics shipper pushes every 15 seconds and its log flusher every
// 3, and both handlers memoised TelemetryResourceMetaForServer only INSIDE a
// single request. The labels being derived — project and environment — change
// only when a resource is created, renamed or moved, so the steady state was a
// constant floor of identical lookups: 500 servers hosting 10 resources each is
// 5,000 resource queries every 15 seconds from the metrics endpoint alone,
// ~330/second, on the same 20-connection pool the reconciler, the long-poll
// wake-ups and the backup scheduler's minute tick need.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/telemetry"
)

// countingTelStore counts every resource-meta lookup that reached the store.
type countingTelStore struct {
	lookups int
	missing bool
}

func (c *countingTelStore) OrgTenant(context.Context, string) (int, error) { return 1, nil }

func (c *countingTelStore) TelemetryResourceMetaForServer(_ context.Context, _, _ string) (store.TelemetryResourceMeta, error) {
	c.lookups++
	if c.missing {
		return store.TelemetryResourceMeta{}, store.ErrNotFound
	}
	return store.TelemetryResourceMeta{ProjectID: "prj_1", EnvironmentID: "env_1"}, nil
}

func (c *countingTelStore) DeployStatsForOrg(context.Context, string, int) (store.DeployStats, error) {
	return store.DeployStats{}, nil
}
func (c *countingTelStore) ConnectedServerCount(context.Context, string) (int, error) { return 0, nil }
func (c *countingTelStore) VerifyDays(context.Context, string, int) ([]store.VerifyDay, error) {
	return nil, nil
}

var _ TelemetryAPI = (*countingTelStore)(nil)

// telemetryServer wires a Server whose metrics and logs sinks both accept
// everything, so the handlers run their full enrichment path.
func telemetryServer(t *testing.T, tel TelemetryAPI) *Server {
	t.Helper()
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(sink.Close)
	fwd := telemetry.New(telemetry.Config{VMWriteURL: sink.URL, LokiURL: sink.URL})
	return New(slog.New(slog.NewTextHandler(io.Discard, nil)), fakePinger{}, &fakeStore{}, &fakeDomain{},
		Options{Telemetry: fwd, TelemetryStore: tel})
}

func postMetricsBatch(t *testing.T, s *Server, srv store.Server, resourceID string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{"samples": []map[string]any{
		{"name": "sigmahub_container_cpu_pct", "labels": map[string]string{"resource": resourceID},
			"value": 12.5, "ts": time.Now().UnixMilli()},
	}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/agent/telemetry/metrics", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), serverCtxKey, srv))
	rec := httptest.NewRecorder()
	s.handleAgentTelemetryMetrics(rec, req)
	return rec
}

func TestTelemetryIngest_ReusesResourceMetaAcrossRequests(t *testing.T) {
	tel := &countingTelStore{}
	s := telemetryServer(t, tel)
	srv := store.Server{ID: "srv_1", OrgID: "org_1"}

	if rec := postMetricsBatch(t, s, srv, "res_1"); rec.Code != http.StatusOK {
		t.Fatalf("first batch = %d: %s", rec.Code, rec.Body.String())
	}
	if tel.lookups != 1 {
		t.Fatalf("first batch made %d resource lookups, want 1", tel.lookups)
	}
	// The agent ships again 15 seconds later, naming the same resource. Its
	// project and environment cannot have moved; nothing should reach the
	// database.
	if rec := postMetricsBatch(t, s, srv, "res_1"); rec.Code != http.StatusOK {
		t.Fatalf("second batch = %d: %s", rec.Code, rec.Body.String())
	}
	if tel.lookups != 1 {
		t.Fatalf("resource meta looked up %d times across two batches, want 1 — every ingest is re-querying the resources table", tel.lookups)
	}
}

// A resource the reporting server does not host must NOT be cached as present,
// and must not be cached as absent either: a resource created moments before
// the batch would otherwise be unlabelable — and therefore invisible — for the
// whole TTL.
func TestTelemetryIngest_DoesNotCacheUnknownResources(t *testing.T) {
	tel := &countingTelStore{missing: true}
	s := telemetryServer(t, tel)
	srv := store.Server{ID: "srv_1", OrgID: "org_1"}

	postMetricsBatch(t, s, srv, "res_elsewhere")
	postMetricsBatch(t, s, srv, "res_elsewhere")
	if tel.lookups != 2 {
		t.Fatalf("unknown resource looked up %d times, want 2 (a negative result must not be memoised across requests)", tel.lookups)
	}

	// And once it does exist, it is labelled immediately rather than after a
	// cache expiry.
	tel.missing = false
	if rec := postMetricsBatch(t, s, srv, "res_elsewhere"); rec.Code != http.StatusOK {
		t.Fatalf("batch after the resource appeared = %d", rec.Code)
	}
	if tel.lookups != 3 {
		t.Fatalf("lookups = %d, want 3", tel.lookups)
	}
}
