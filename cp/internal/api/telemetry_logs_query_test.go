package api

// SIGMA-349: handleLogsQuery builds its LogQL stream selector by concatenation.
// Every label value taken from the query string is checked against
// logQLLabelValue first — except orgId, the one label that decides WHICH
// TENANT'S logs come back, which was interpolated straight into org="…".
//
// Nothing could reach it with a metacharacter: requireService refuses a
// token/path org mismatch, and orgIDPattern pins ids to [A-Za-z0-9_-] when they
// are provisioned. But that invariant lives in another file on another request,
// and it is the only thing standing between a wider org-id scheme (a vanity
// slug, an SSO-derived id, an import writing org_tenants directly) and
// cross-tenant log disclosure. This pins it where the concatenation happens.

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLogsQueryRefusesAnOrgIdThatCouldEscapeTheSelector(t *testing.T) {
	s := telemetryServer(t, &countingTelStore{})

	for _, orgID := range []string{
		`org_1", org=~".+`, // close the quote, widen the matcher
		`org_1"}|="`,       // close the selector outright
		`org_1\`,           // trailing escape
		"",                 // no id at all
	} {
		req := httptest.NewRequest(http.MethodGet, "/v1/orgs/x/logs/query", nil)
		req.SetPathValue("orgId", orgID)
		rec := httptest.NewRecorder()
		s.handleLogsQuery(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("orgId %q reached the sink with status %d, want 400 — a selector must not be assembled from an unchecked org id", orgID, rec.Code)
		}
	}
}

func TestLogsQueryStillServesAWellFormedOrgId(t *testing.T) {
	s := telemetryServer(t, &countingTelStore{})
	req := httptest.NewRequest(http.MethodGet, "/v1/orgs/org_p_abc123/logs/query", nil)
	req.SetPathValue("orgId", "org_p_abc123")
	rec := httptest.NewRecorder()
	s.handleLogsQuery(rec, req)
	if rec.Code == http.StatusBadRequest {
		t.Fatalf("a normally-provisioned org id was rejected: %s", rec.Body.String())
	}
}
