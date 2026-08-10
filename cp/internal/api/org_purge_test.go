package api

// SIGMA-284: the tenant-erasure route's gate and its confirmation.
//
// Two things about DELETE /v1/orgs/{orgId} have to hold, and neither is
// something the store can enforce: the org's own credential must NOT be able
// to trigger it (a stolen dashboard token would otherwise destroy the tenant it
// was stolen from, including the key material that makes its backups
// restorable), and the body must name the same org as the path, so a purge is
// never one mistyped URL away.

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

func deleteAsToken(s *Server, token, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("DELETE", path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestPurgeOrgRouteGateAndConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		token  string
		body   string
		want   int
		purged int
	}{
		{"provision token purges", testProvisionToken, `{"confirmOrgId":"org_1"}`, 200, 1},
		{"no credential", "", `{"confirmOrgId":"org_1"}`, 401, 0},
		{"an org's own Org Admin token cannot erase it", "sst_admin_org1", `{"confirmOrgId":"org_1"}`, 401, 0},
		{"confirmation must match the path", testProvisionToken, `{"confirmOrgId":"org_2"}`, 400, 0},
		{"confirmation is required", testProvisionToken, `{}`, 400, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fd := &fakeDomain{}
			fs := &fakeStore{serviceTokens: map[string]store.ServicePrincipal{
				"sst_admin_org1": {ID: "st_1", OrgID: "org_1", Name: "web", Role: store.RoleOrgAdmin},
			}}
			s := newTestServerWith(t, fs, fd)
			rec := deleteAsToken(s, tc.token, "/v1/orgs/org_1", tc.body)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.want, rec.Body.String())
			}
			if len(fd.purged) != tc.purged {
				t.Fatalf("PurgeOrg called %d times, want %d", len(fd.purged), tc.purged)
			}
			if tc.purged == 1 && fd.purged[0] != "org_1" {
				t.Fatalf("purged %q, want org_1", fd.purged[0])
			}
		})
	}
}
