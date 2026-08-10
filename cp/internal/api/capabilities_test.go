package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

func capabilitiesServer(t *testing.T, dbEngines, s3Engines []string) *Server {
	t.Helper()
	return New(slog.Default(), fakePinger{}, &fakeStore{}, &fakeDomain{}, Options{
		DevServiceToken: testServiceToken,
		ProvisionToken:  testProvisionToken,
		DBEngines:       dbEngines,
		S3Engines:       s3Engines,
	})
}

func readCapabilities(t *testing.T, s *Server) capabilitiesResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/orgs/org_1/capabilities", nil)
	req.Header.Set("Authorization", "Bearer "+testServiceToken)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET capabilities = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var body capabilitiesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unreadable capabilities response %s: %v", rec.Body.String(), err)
	}
	return body
}

// The engines a deployment turned off are the control plane's own fact, and
// until it published them the wizard could only guess — it reads the generated
// catalog, which is the full set this codebase can provision, not the set this
// control plane will accept. The operator found out at create, with a 422, after
// the dialog had closed (SIGMA-268).
func TestCapabilitiesPublishesTheEnabledEngineSets(t *testing.T) {
	s := capabilitiesServer(t, []string{"postgres", "redis"}, []string{"minio"})
	body := readCapabilities(t, s)

	if !slices.Equal(body.DBEngines, []string{"postgres", "redis"}) {
		t.Errorf("dbEngines = %v, want the configured allowlist", body.DBEngines)
	}
	if !slices.Equal(body.S3Engines, []string{"minio"}) {
		t.Errorf("s3Engines = %v, want the configured allowlist", body.S3Engines)
	}
	// And the answer is the catalog's order, not the operator's typing: two
	// control planes with the same engines enabled must answer identically.
	reordered := capabilitiesServer(t, []string{"redis", "postgres"}, nil)
	if got := readCapabilities(t, reordered).DBEngines; !slices.Equal(got, body.DBEngines) {
		t.Errorf("the same two engines listed in the other order answered %v, want %v", got, body.DBEngines)
	}
}

// An unconfigured allowlist means "not restricted" everywhere else in the
// control plane (config.parseEngineList defaults to the whole catalog, the store
// treats a nil allowlist as everything enabled), and it has to mean the same
// here. Publishing an empty list would tell the wizard that nothing can be
// created.
func TestCapabilitiesWithNoAllowlistPublishesTheWholeCatalog(t *testing.T) {
	body := readCapabilities(t, capabilitiesServer(t, nil, nil))

	if !slices.Equal(body.DBEngines, store.DBEngineKinds()) {
		t.Errorf("dbEngines = %v, want the full catalog %v", body.DBEngines, store.DBEngineKinds())
	}
	if !slices.Equal(body.S3Engines, store.S3EngineNames()) {
		t.Errorf("s3Engines = %v, want the full catalog %v", body.S3Engines, store.S3EngineNames())
	}
}

// It is org-scoped dashboard data, so it lives behind the same token as the
// rest of the dashboard's reads.
func TestCapabilitiesRequiresACredential(t *testing.T) {
	s := capabilitiesServer(t, nil, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/orgs/org_1/capabilities", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET capabilities = %d, want 401; body %s", rec.Code, rec.Body.String())
	}
}
