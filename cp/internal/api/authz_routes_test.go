package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// authz_routes_test.go is the standing answer to SIGMA-322: before it, the role
// required by a /v1/orgs route was asserted for exactly three of the 114 routes
// that have one. Changing `store.RoleProjectAdmin` to `store.RoleDeveloper` on
// one line of routes() — the kind of slip a rebase or a copy of the line above
// produces — handed every Developer-role token in the org the SSH-lockout and
// firewall controls, and no test in the repository noticed.
//
// The expectations below are a CHECKED-IN table, not something derived from the
// code: derived expectations agree with whatever the code says and assert
// nothing. Adding a route therefore fails this test until someone writes down,
// deliberately, what role that route needs. Deleting a route fails it too.
//
// The table is enforced two ways, because each catches what the other cannot:
//
//   - Textually, by parsing routes() (see parseRegisteredRoutes). This is what
//     lets the test see a route that is registered with NO auth wrapper at all
//     — a live handler that answers before any credential is checked is
//     invisible to a request-level test that only ever sends valid tokens.
//   - Behaviourally, by driving real requests through the mux with tokens one
//     tier below the required role, with a token from another org, and with no
//     token. Those three all short-circuit inside requireService, so they never
//     reach a handler and cannot be tripped up by the nil sub-APIs a handler
//     unit-test server is built with.

type routeKey struct {
	Method  string
	Pattern string
}

func (k routeKey) String() string { return k.Method + " " + k.Pattern }

// orgRouteMinRole is the minimum role every org-scoped route requires. Keep it
// sorted the way the test prints it (method, then pattern) so a regenerated
// dump can be diffed against it. A new route MUST get an entry here; think
// about the tier before you add one — "the line above says ProjectAdmin" is how
// SIGMA-322 happened in the first place.
var orgRouteMinRole = map[routeKey]store.Role{
	{"DELETE", "/v1/orgs/{orgId}/alert-channels/{channelId}"}:                  store.RoleOrgAdmin,
	{"DELETE", "/v1/orgs/{orgId}/backup-targets/{targetId}"}:                   store.RoleProjectAdmin,
	{"DELETE", "/v1/orgs/{orgId}/clusters/{clusterId}"}:                        store.RoleProjectAdmin,
	{"DELETE", "/v1/orgs/{orgId}/clusters/{clusterId}/nodes/{serverId}"}:       store.RoleProjectAdmin,
	{"DELETE", "/v1/orgs/{orgId}/domains/{domainId}"}:                          store.RoleProjectAdmin,
	{"DELETE", "/v1/orgs/{orgId}/environments/{envId}"}:                        store.RoleProjectAdmin,
	{"DELETE", "/v1/orgs/{orgId}/environments/{envId}/servers/{serverId}"}:     store.RoleProjectAdmin,
	{"DELETE", "/v1/orgs/{orgId}/git/branch-maps/{mapId}"}:                     store.RoleProjectAdmin,
	{"DELETE", "/v1/orgs/{orgId}/git/connections/{connId}"}:                    store.RoleProjectAdmin,
	{"DELETE", "/v1/orgs/{orgId}/git/integration/{installationId}"}:            store.RoleProjectAdmin,
	{"DELETE", "/v1/orgs/{orgId}/projects/{projectId}"}:                        store.RoleProjectAdmin,
	{"DELETE", "/v1/orgs/{orgId}/registry"}:                                    store.RoleOrgAdmin,
	{"DELETE", "/v1/orgs/{orgId}/resources/{resourceId}"}:                      store.RoleProjectAdmin,
	{"DELETE", "/v1/orgs/{orgId}/resources/{resourceId}/buckets/{bucket}"}:     store.RoleProjectAdmin,
	{"DELETE", "/v1/orgs/{orgId}/secrets/{secretId}"}:                          store.RoleProjectAdmin,
	{"DELETE", "/v1/orgs/{orgId}/servers/{serverId}"}:                          store.RoleProjectAdmin,
	{"DELETE", "/v1/orgs/{orgId}/service-tokens/{tokenId}"}:                    store.RoleOrgAdmin,
	{"GET", "/v1/orgs/{orgId}/alert-channels"}:                                 store.RoleDeveloper,
	{"GET", "/v1/orgs/{orgId}/audit"}:                                          store.RoleDeveloper,
	{"GET", "/v1/orgs/{orgId}/backup-repo-keys"}:                               store.RoleOrgAdmin,
	{"GET", "/v1/orgs/{orgId}/backup-targets"}:                                 store.RoleDeveloper,
	{"GET", "/v1/orgs/{orgId}/backups/verify-days"}:                            store.RoleDeveloper,
	{"GET", "/v1/orgs/{orgId}/beta-metrics"}:                                   store.RoleDeveloper,
	{"GET", "/v1/orgs/{orgId}/billing"}:                                        store.RoleDeveloper,
	{"GET", "/v1/orgs/{orgId}/capabilities"}:                                   store.RoleDeveloper,
	{"GET", "/v1/orgs/{orgId}/clusters"}:                                       store.RoleDeveloper,
	{"GET", "/v1/orgs/{orgId}/deployments"}:                                    store.RoleDeveloper,
	{"GET", "/v1/orgs/{orgId}/deployments/{deploymentId}/logs"}:                store.RoleDeveloper,
	{"GET", "/v1/orgs/{orgId}/domains/{domainId}/dns"}:                         store.RoleDeveloper,
	{"GET", "/v1/orgs/{orgId}/environments/{envId}/servers"}:                   store.RoleDeveloper,
	{"GET", "/v1/orgs/{orgId}/git/app"}:                                        store.RoleDeveloper,
	{"GET", "/v1/orgs/{orgId}/git/connections"}:                                store.RoleDeveloper,
	{"GET", "/v1/orgs/{orgId}/git/connections/{connId}"}:                       store.RoleDeveloper,
	{"GET", "/v1/orgs/{orgId}/git/connections/{connId}/previews"}:              store.RoleDeveloper,
	{"GET", "/v1/orgs/{orgId}/git/deploy-requests"}:                            store.RoleDeveloper,
	{"GET", "/v1/orgs/{orgId}/git/integration"}:                                store.RoleDeveloper,
	{"GET", "/v1/orgs/{orgId}/git/repos"}:                                      store.RoleDeveloper,
	{"GET", "/v1/orgs/{orgId}/llm/engines"}:                                    store.RoleDeveloper,
	{"GET", "/v1/orgs/{orgId}/llm/models"}:                                     store.RoleDeveloper,
	{"GET", "/v1/orgs/{orgId}/llm/models/resolve"}:                             store.RoleDeveloper,
	{"GET", "/v1/orgs/{orgId}/logs/query"}:                                     store.RoleDeveloper,
	{"GET", "/v1/orgs/{orgId}/metrics/query"}:                                  store.RoleDeveloper,
	{"GET", "/v1/orgs/{orgId}/projects"}:                                       store.RoleDeveloper,
	{"GET", "/v1/orgs/{orgId}/projects/{projectId}"}:                           store.RoleDeveloper,
	{"GET", "/v1/orgs/{orgId}/projects/{projectId}/environments"}:              store.RoleDeveloper,
	{"GET", "/v1/orgs/{orgId}/projects/{projectId}/secrets"}:                   store.RoleDeveloper,
	{"GET", "/v1/orgs/{orgId}/registry"}:                                       store.RoleDeveloper,
	{"GET", "/v1/orgs/{orgId}/resources"}:                                      store.RoleDeveloper,
	{"GET", "/v1/orgs/{orgId}/resources/{resourceId}/backup-runs"}:             store.RoleDeveloper,
	{"GET", "/v1/orgs/{orgId}/resources/{resourceId}/buckets"}:                 store.RoleDeveloper,
	{"GET", "/v1/orgs/{orgId}/resources/{resourceId}/buckets/{bucket}/key"}:    store.RoleProjectAdmin,
	{"GET", "/v1/orgs/{orgId}/resources/{resourceId}/compose"}:                 store.RoleDeveloper,
	{"GET", "/v1/orgs/{orgId}/resources/{resourceId}/database"}:                store.RoleDeveloper,
	{"GET", "/v1/orgs/{orgId}/resources/{resourceId}/database/connection"}:     store.RoleProjectAdmin,
	{"GET", "/v1/orgs/{orgId}/resources/{resourceId}/deployments"}:             store.RoleDeveloper,
	{"GET", "/v1/orgs/{orgId}/resources/{resourceId}/domains"}:                 store.RoleDeveloper,
	{"GET", "/v1/orgs/{orgId}/resources/{resourceId}/llm"}:                     store.RoleDeveloper,
	{"GET", "/v1/orgs/{orgId}/resources/{resourceId}/rollback-targets"}:        store.RoleDeveloper,
	{"GET", "/v1/orgs/{orgId}/resources/{resourceId}/s3"}:                      store.RoleDeveloper,
	{"GET", "/v1/orgs/{orgId}/resources/{resourceId}/s3/connection"}:           store.RoleProjectAdmin,
	{"GET", "/v1/orgs/{orgId}/secrets/{secretId}/value"}:                       store.RoleProjectAdmin,
	{"GET", "/v1/orgs/{orgId}/servers"}:                                        store.RoleDeveloper,
	{"GET", "/v1/orgs/{orgId}/servers/{serverId}"}:                             store.RoleDeveloper,
	{"GET", "/v1/orgs/{orgId}/servers/{serverId}/metrics"}:                     store.RoleDeveloper,
	{"GET", "/v1/orgs/{orgId}/service-tokens"}:                                 store.RoleOrgAdmin,
	{"PATCH", "/v1/orgs/{orgId}/environments/{envId}"}:                         store.RoleProjectAdmin,
	{"PATCH", "/v1/orgs/{orgId}/projects/{projectId}"}:                         store.RoleProjectAdmin,
	{"PATCH", "/v1/orgs/{orgId}/resources/{resourceId}/backup-policy"}:         store.RoleProjectAdmin,
	{"POST", "/v1/orgs/{orgId}/alert-channels"}:                                store.RoleOrgAdmin,
	{"POST", "/v1/orgs/{orgId}/alert-channels/{channelId}/test"}:               store.RoleOrgAdmin,
	{"POST", "/v1/orgs/{orgId}/backup-targets"}:                                store.RoleProjectAdmin,
	{"POST", "/v1/orgs/{orgId}/billing/checkout"}:                              store.RoleOrgAdmin,
	{"POST", "/v1/orgs/{orgId}/billing/portal"}:                                store.RoleOrgAdmin,
	{"POST", "/v1/orgs/{orgId}/bootstrap-tokens"}:                              store.RoleProjectAdmin,
	{"POST", "/v1/orgs/{orgId}/clusters"}:                                      store.RoleProjectAdmin,
	{"POST", "/v1/orgs/{orgId}/clusters/{clusterId}/nodes"}:                    store.RoleProjectAdmin,
	{"POST", "/v1/orgs/{orgId}/environments/{envId}/servers"}:                  store.RoleProjectAdmin,
	{"POST", "/v1/orgs/{orgId}/git/branch-maps/{mapId}/promote"}:               store.RoleProjectAdmin,
	{"POST", "/v1/orgs/{orgId}/git/connections"}:                               store.RoleProjectAdmin,
	{"POST", "/v1/orgs/{orgId}/git/connections/{connId}/installation"}:         store.RoleProjectAdmin,
	{"POST", "/v1/orgs/{orgId}/git/detect"}:                                    store.RoleDeveloper,
	{"POST", "/v1/orgs/{orgId}/git/integration"}:                               store.RoleProjectAdmin,
	{"POST", "/v1/orgs/{orgId}/git/repos/select"}:                              store.RoleProjectAdmin,
	{"POST", "/v1/orgs/{orgId}/projects"}:                                      store.RoleProjectAdmin,
	{"POST", "/v1/orgs/{orgId}/projects/{projectId}/environments"}:             store.RoleProjectAdmin,
	{"POST", "/v1/orgs/{orgId}/projects/{projectId}/secrets"}:                  store.RoleProjectAdmin,
	{"POST", "/v1/orgs/{orgId}/resources"}:                                     store.RoleProjectAdmin,
	{"POST", "/v1/orgs/{orgId}/resources/{resourceId}/backup-repo-key/export"}: store.RoleOrgAdmin,
	{"POST", "/v1/orgs/{orgId}/resources/{resourceId}/buckets"}:                store.RoleProjectAdmin,
	{"POST", "/v1/orgs/{orgId}/resources/{resourceId}/buckets/{bucket}/key"}:   store.RoleProjectAdmin,
	{"POST", "/v1/orgs/{orgId}/resources/{resourceId}/database/expose"}:        store.RoleProjectAdmin,
	{"POST", "/v1/orgs/{orgId}/resources/{resourceId}/deploy"}:                 store.RoleProjectAdmin,
	{"POST", "/v1/orgs/{orgId}/resources/{resourceId}/domains"}:                store.RoleProjectAdmin,
	{"POST", "/v1/orgs/{orgId}/resources/{resourceId}/restore"}:                store.RoleProjectAdmin,
	{"POST", "/v1/orgs/{orgId}/resources/{resourceId}/restore-pitr"}:           store.RoleProjectAdmin,
	{"POST", "/v1/orgs/{orgId}/resources/{resourceId}/rollback"}:               store.RoleProjectAdmin,
	{"POST", "/v1/orgs/{orgId}/secrets/rotate-dek"}:                            store.RoleOrgAdmin,
	{"POST", "/v1/orgs/{orgId}/secrets/rotate-kek"}:                            store.RoleOrgAdmin,
	{"POST", "/v1/orgs/{orgId}/servers/provision"}:                             store.RoleProjectAdmin,
	{"POST", "/v1/orgs/{orgId}/servers/{serverId}/agent-update"}:               store.RoleProjectAdmin,
	{"POST", "/v1/orgs/{orgId}/servers/{serverId}/confirm-tokens"}:             store.RoleProjectAdmin,
	{"POST", "/v1/orgs/{orgId}/servers/{serverId}/decommission"}:               store.RoleProjectAdmin,
	{"POST", "/v1/orgs/{orgId}/servers/{serverId}/destructive-ops"}:            store.RoleProjectAdmin,
	{"POST", "/v1/orgs/{orgId}/servers/{serverId}/hardening"}:                  store.RoleProjectAdmin,
	{"POST", "/v1/orgs/{orgId}/servers/{serverId}/proxy-role"}:                 store.RoleProjectAdmin,
	{"POST", "/v1/orgs/{orgId}/servers/{serverId}/reissue-token"}:              store.RoleProjectAdmin,
	{"POST", "/v1/orgs/{orgId}/servers/{serverId}/rename"}:                     store.RoleProjectAdmin,
	{"POST", "/v1/orgs/{orgId}/servers/{serverId}/revoke-token"}:               store.RoleProjectAdmin,
	{"POST", "/v1/orgs/{orgId}/servers/{serverId}/type"}:                       store.RoleProjectAdmin,
	{"POST", "/v1/orgs/{orgId}/service-tokens/{tokenId}/rotate"}:               store.RoleOrgAdmin,
	{"PUT", "/v1/orgs/{orgId}/alert-channels/{channelId}/rules"}:               store.RoleOrgAdmin,
	{"PUT", "/v1/orgs/{orgId}/git/connections/{connId}/branches"}:              store.RoleProjectAdmin,
	{"PUT", "/v1/orgs/{orgId}/git/connections/{connId}/previews"}:              store.RoleProjectAdmin,
	{"PUT", "/v1/orgs/{orgId}/registry"}:                                       store.RoleOrgAdmin,
	{"PUT", "/v1/orgs/{orgId}/resources/{resourceId}/buckets/{bucket}/quota"}:  store.RoleProjectAdmin,
	{"PUT", "/v1/orgs/{orgId}/resources/{resourceId}/compose/placements"}:      store.RoleProjectAdmin,
	{"PUT", "/v1/orgs/{orgId}/secrets/{secretId}"}:                             store.RoleProjectAdmin,
}

// provisionTokenOrgRoutes are the only /v1/orgs routes that are NOT gated by a
// service token: org creation and its inverse, tenant erasure. Both hold the
// deployment-wide provision credential instead, because at org-creation time no
// org-scoped token exists yet and after erasure none should. Anything else that
// turns up outside requireService is a bug — see TestNoOrgRouteEscapesRequireService.
var provisionTokenOrgRoutes = map[routeKey]bool{
	{"POST", "/v1/orgs"}:           true,
	{"DELETE", "/v1/orgs/{orgId}"}: true,
}

// registeredRoute is what parsing routes() recovers about one registration.
type registeredRoute struct {
	key routeKey
	// guard is the name of the wrapper the handler is registered inside —
	// "requireService", "requireAgent", "requireProvision" — or "" when the
	// handler is registered raw (health probes, the installer, the webhooks).
	guard string
	// role is the store.Role handed to requireService; "" for any other guard.
	role store.Role
}

// roleIdents maps the identifier as written in routes() to its value, so the
// test reads the constant the source names rather than re-deriving it.
var roleIdents = map[string]store.Role{
	"RoleDeveloper":    store.RoleDeveloper,
	"RoleProjectAdmin": store.RoleProjectAdmin,
	"RoleOrgAdmin":     store.RoleOrgAdmin,
}

// parseRegisteredRoutes reads routes() out of server.go and reports every
// s.mux.HandleFunc registration with the guard it is wrapped in. Parsing the
// source is deliberate: http.ServeMux exposes no way to enumerate what was
// registered on it, and a route registered with no guard at all — the failure
// this test exists to catch alongside a downgraded role — cannot be observed by
// sending requests, because there is nothing to send that would be refused.
func parseRegisteredRoutes(t *testing.T) []registeredRoute {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "server.go", nil, 0)
	if err != nil {
		t.Fatalf("parse server.go: %v", err)
	}

	var routesFn *ast.FuncDecl
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == "routes" && fn.Recv != nil {
			routesFn = fn
			break
		}
	}
	if routesFn == nil {
		t.Fatal("no func (s *Server) routes() in server.go — this test cannot see the route table")
	}

	var out []registeredRoute
	ast.Inspect(routesFn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || calleeName(call) != "HandleFunc" || len(call.Args) != 2 {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			t.Errorf("HandleFunc pattern at %s is not a string literal; this test can no longer read the route table", fset.Position(call.Pos()))
			return true
		}
		pattern, err := strconv.Unquote(lit.Value)
		if err != nil {
			t.Errorf("unquote %s: %v", lit.Value, err)
			return true
		}
		method, path, found := strings.Cut(pattern, " ")
		if !found {
			t.Errorf("route %q has no method prefix", pattern)
			return true
		}

		r := registeredRoute{key: routeKey{Method: method, Pattern: path}}
		if guard, ok := call.Args[1].(*ast.CallExpr); ok {
			r.guard = calleeName(guard)
			if r.guard == "requireService" && len(guard.Args) == 2 {
				if sel, ok := guard.Args[0].(*ast.SelectorExpr); ok {
					role, known := roleIdents[sel.Sel.Name]
					if !known {
						t.Errorf("%s: requireService with unknown role identifier %s", r.key, sel.Sel.Name)
					}
					r.role = role
				} else {
					t.Errorf("%s: requireService min role is not a store.Role constant", r.key)
				}
			}
		}
		out = append(out, r)
		return true
	})
	return out
}

// calleeName is the final selector of a call's function expression, e.g.
// "HandleFunc" for s.mux.HandleFunc(...) and "requireService" for
// s.requireService(...).
func calleeName(call *ast.CallExpr) string {
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		return sel.Sel.Name
	}
	if id, ok := call.Fun.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// orgScoped reports whether a path is one of the tenant-scoped routes this
// test governs: everything under /v1/orgs, which is the whole dashboard-facing
// surface. /v1/agent/* is authenticated by agent token and /healthz, /metrics,
// /install.sh and the webhooks are deliberately public — see routes().
func orgScoped(path string) bool {
	return path == "/v1/orgs" || strings.HasPrefix(path, "/v1/orgs/")
}

func TestEveryOrgRouteHasExpectedMinimumRole(t *testing.T) {
	registered := parseRegisteredRoutes(t)

	got := map[routeKey]store.Role{}
	for _, r := range registered {
		if !orgScoped(r.key.Pattern) || provisionTokenOrgRoutes[r.key] {
			continue
		}
		if _, dup := got[r.key]; dup {
			t.Errorf("route %s registered twice", r.key)
		}
		got[r.key] = r.role
	}

	// Every registered route must be in the table, at the role the table says.
	// A missing entry is the interesting case: it means a route shipped without
	// anybody writing down what role it needs.
	for key, role := range got {
		want, ok := orgRouteMinRole[key]
		if !ok {
			t.Errorf("route %s is registered but has no entry in orgRouteMinRole; add one naming the role it needs (it is currently %q)", key, role)
			continue
		}
		if role != want {
			t.Errorf("route %s requires %q, expected %q", key, role, want)
		}
	}
	for key := range orgRouteMinRole {
		if _, ok := got[key]; !ok {
			t.Errorf("orgRouteMinRole has an entry for %s but no such route is registered; remove the entry if the route is gone", key)
		}
	}

	if t.Failed() {
		t.Log("routes as currently registered:\n" + dumpOrgRoutes(registered))
	}

	// The table above is a claim about the source. This drives it: for every
	// route above Developer, a token exactly one tier below the required role
	// must be refused. Nothing here reaches a handler — requireService answers
	// 403 first — so it is safe to fire at all 114 routes against a server whose
	// sub-APIs are nil.
	s := newTestServer(t, &fakeStore{serviceTokens: map[string]store.ServicePrincipal{
		"sst_dev":       {ID: "st_1", OrgID: "org_1", Name: "dev", Role: store.RoleDeveloper},
		"sst_projadmin": {ID: "st_2", OrgID: "org_1", Name: "pa", Role: store.RoleProjectAdmin},
		"sst_other_org": {ID: "st_3", OrgID: "org_2", Name: "other", Role: store.RoleOrgAdmin},
	}})
	tokenForRole := map[store.Role]string{
		store.RoleDeveloper:    "sst_dev",
		store.RoleProjectAdmin: "sst_projadmin",
	}
	below := map[store.Role]store.Role{
		store.RoleProjectAdmin: store.RoleDeveloper,
		store.RoleOrgAdmin:     store.RoleProjectAdmin,
	}
	for _, key := range sortedKeys(orgRouteMinRole) {
		want := orgRouteMinRole[key]
		t.Run(key.String(), func(t *testing.T) {
			// No credential at all is 401 on every org route.
			if code := doRoute(s, key, ""); code != http.StatusUnauthorized {
				t.Errorf("no token: status = %d, want 401", code)
			}
			// A valid token belonging to another org is 403 on every org
			// route — the tenant check runs before the role check.
			if code := doRoute(s, key, "sst_other_org"); code != http.StatusForbidden {
				t.Errorf("token from another org: status = %d, want 403", code)
			}
			lower, ok := below[want]
			if !ok {
				// Developer is the floor; there is no weaker token to refuse
				// with. The source assertion above is what pins these.
				return
			}
			if code := doRoute(s, key, tokenForRole[lower]); code != http.StatusForbidden {
				t.Errorf("%s token on a route that requires %s: status = %d, want 403 — the role gate was widened", lower, want, code)
			}
		})
	}
}

// TestNoOrgRouteEscapesRequireService is the other half of SIGMA-322: a role
// table asserts nothing about a route that was registered with no gate at all.
func TestNoOrgRouteEscapesRequireService(t *testing.T) {
	for _, r := range parseRegisteredRoutes(t) {
		if !orgScoped(r.key.Pattern) {
			continue
		}
		if provisionTokenOrgRoutes[r.key] {
			if r.guard != "requireProvision" {
				t.Errorf("route %s is guarded by %q, want requireProvision", r.key, r.guard)
			}
			continue
		}
		if r.guard != "requireService" {
			what := r.guard
			if what == "" {
				what = "nothing"
			}
			t.Errorf("route %s is guarded by %s, want requireService; an org-scoped route that answers without a service token is a tenant-isolation hole", r.key, what)
		}
	}
}

// doRoute fires one request at a registered pattern and reports the status.
// Path placeholders are filled with values that need not exist: every assertion
// made with it is answered by requireService before any handler runs.
func doRoute(s *Server, key routeKey, token string) int {
	path := strings.ReplaceAll(key.Pattern, "{orgId}", "org_1")
	for {
		open := strings.Index(path, "{")
		if open < 0 {
			break
		}
		closeIdx := strings.Index(path[open:], "}")
		if closeIdx < 0 {
			break
		}
		path = path[:open] + "x" + path[open+closeIdx+1:]
	}
	var body io.Reader
	switch key.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		body = strings.NewReader(`{}`)
	}
	req := httptest.NewRequest(key.Method, path, body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec.Code
}

func sortedKeys(m map[routeKey]store.Role) []routeKey {
	keys := make([]routeKey, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Method != keys[j].Method {
			return keys[i].Method < keys[j].Method
		}
		return keys[i].Pattern < keys[j].Pattern
	})
	return keys
}

// dumpOrgRoutes renders the registered org routes as Go source for the table
// above, so regenerating it after a deliberate route change is a copy rather
// than an afternoon of hand-editing.
func dumpOrgRoutes(routes []registeredRoute) string {
	m := map[routeKey]store.Role{}
	for _, r := range routes {
		if orgScoped(r.key.Pattern) && !provisionTokenOrgRoutes[r.key] {
			m[r.key] = r.role
		}
	}
	var b strings.Builder
	for _, k := range sortedKeys(m) {
		ident := "??"
		for name, role := range roleIdents {
			if role == m[k] {
				ident = name
			}
		}
		b.WriteString("\t{" + strconv.Quote(k.Method) + ", " + strconv.Quote(k.Pattern) + "}: store." + ident + ",\n")
	}
	return b.String()
}
