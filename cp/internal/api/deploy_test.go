package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// recReconcile records ReconcileAsync calls so tests can assert a mutation
// re-rendered the right server.
type recReconcile struct{ calls [][2]string }

func (r *recReconcile) ReconcileAsync(orgID, serverID string) {
	r.calls = append(r.calls, [2]string{orgID, serverID})
}

// TestDeploymentEndpoints pins the P1-9 read/rollback/log surface: list + rollback
// targets are member-visible, a rollback is Project Admin+ and re-renders the
// server, and the log endpoint returns a cursor snapshot.
func TestDeploymentEndpoints(t *testing.T) {
	rec := &recReconcile{}
	s := New(slog.Default(), fakePinger{}, &fakeStore{}, &fakeDomain{}, Options{
		DevServiceToken: testServiceToken,
		Reconcile:       rec,
	})

	do := func(method, path, body string) *httptest.ResponseRecorder {
		var rdr *strings.Reader
		if body == "" {
			rdr = strings.NewReader("")
		} else {
			rdr = strings.NewReader(body)
		}
		req := httptest.NewRequest(method, path, rdr)
		req.Header.Set("Authorization", "Bearer "+testServiceToken)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		return w
	}

	if got := do("GET", "/v1/orgs/org_1/resources/res_1/deployments", ""); got.Code != http.StatusOK {
		t.Fatalf("list deployments: status=%d", got.Code)
	}
	if got := do("GET", "/v1/orgs/org_1/resources/res_1/rollback-targets", ""); got.Code != http.StatusOK {
		t.Fatalf("rollback targets: status=%d", got.Code)
	}

	// Rollback needs a target id.
	if got := do("POST", "/v1/orgs/org_1/resources/res_1/rollback", `{}`); got.Code != http.StatusBadRequest {
		t.Fatalf("rollback without target: status=%d, want 400", got.Code)
	}

	rb := do("POST", "/v1/orgs/org_1/resources/res_1/rollback", `{"targetDeploymentId":"dep_old"}`)
	if rb.Code != http.StatusCreated {
		t.Fatalf("rollback: status=%d", rb.Code)
	}
	var dep map[string]any
	_ = json.Unmarshal(rb.Body.Bytes(), &dep)
	if dep["trigger"] != "rollback" || dep["rollbackOf"] != "dep_old" {
		t.Fatalf("rollback deployment = %v", dep)
	}
	if len(rec.calls) != 1 || rec.calls[0] != [2]string{"org_1", "srv_1"} {
		t.Fatalf("rollback must reconcile the server, got %v", rec.calls)
	}

	// Log snapshot (JSON cursor form).
	logs := do("GET", "/v1/orgs/org_1/deployments/dep_1/logs", "")
	if logs.Code != http.StatusOK {
		t.Fatalf("deploy logs: status=%d", logs.Code)
	}
	var snap map[string]any
	_ = json.Unmarshal(logs.Body.Bytes(), &snap)
	if _, ok := snap["deployment"]; !ok {
		t.Fatalf("log snapshot missing deployment: %s", logs.Body.String())
	}
}
