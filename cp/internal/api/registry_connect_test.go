package api

// The HTTP boundary of the connect flow (SIGMA-202) and of the two exits from
// an incompatible enrollment (SIGMA-203).
//
// These are handler tests, so they prove the WIRING the integration suite takes
// for granted: that the fields the connect form now sends reach the store, that
// the exits' path values and bodies are not dropped on the way through, and
// that a store refusal keeps its status code instead of collapsing into a 500.
// Each of those is one line in a handler, and each would leave every other test
// in the repository green if it were deleted.

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// devActor is the name the dev service token acts under; the exits audit it, so
// it is what the handler must forward.
const devActor = "dev-service-token"

// newTestServerWith is newTestServer with a caller-supplied domain fake, so a
// test can read back what the handler asked the store to do.
func newTestServerWith(t *testing.T, fs *fakeStore, fd *fakeDomain) *Server {
	t.Helper()
	if fs == nil {
		fs = &fakeStore{}
	}
	if fd == nil {
		fd = &fakeDomain{}
	}
	return New(slog.Default(), fakePinger{}, fs, fd, Options{
		DevServiceToken: testServiceToken,
		ProvisionToken:  testProvisionToken,
	})
}

// postAsToken is postJSON with a caller-chosen credential, for the role gates.
func postAsToken(s *Server, token, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// The connect form is two inputs now: the host address and the type. Nothing
// else may be REQUIRED, and the address must survive the trip — it is the
// server's initial public endpoint and, until the agent reports a hostname, its
// name.
func TestProvisionAcceptsTheTwoInputConnectForm(t *testing.T) {
	fs := &fakeStore{}
	s := newTestServerWith(t, fs, nil)

	rec := postJSON(t, s, "/v1/orgs/org_1/servers/provision",
		`{"type":"gpu","hostIp":"203.0.113.9"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("provision with no name/provider/region/distro → %d, want 201; body %s", rec.Code, rec.Body)
	}
	if len(fs.provisioned) != 1 {
		t.Fatalf("store saw %d provision calls", len(fs.provisioned))
	}
	in := fs.provisioned[0]
	if in.Type != "gpu" {
		t.Errorf("type = %q, want gpu", in.Type)
	}
	if in.HostIP != "203.0.113.9" {
		t.Errorf("hostIp = %q — the address the operator typed must reach the store", in.HostIP)
	}
	if in.Name != "" {
		t.Errorf("name = %q, want empty so registration can use the reported hostname", in.Name)
	}

	// The install command is minted in the same response, so the dialog can
	// show it immediately and start waiting for the agent.
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"serverId", "token", "expiresAt", "bootstrapPubkey"} {
		if out[key] == nil || out[key] == "" {
			t.Errorf("response has no %s: %v", key, out)
		}
	}
}

// The type-change exit. The handler's whole job is validate → delegate →
// re-read, and each step is one line.
func TestSetServerTypeExit(t *testing.T) {
	fd := &fakeDomain{}
	fs := &fakeStore{servers: []store.Server{{
		ID: "srv_1", OrgID: "org_1", Name: "misfiled", Type: "general",
		Status: store.ServerStatusRunning, IncompatibleReasons: []store.FailedRequirement{},
	}}}
	s := newTestServerWith(t, fs, fd)

	rec := postJSON(t, s, "/v1/orgs/org_1/servers/srv_1/type", `{"type":"general"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("type → %d, want 200; body %s", rec.Code, rec.Body)
	}
	if len(fd.typeCalls) != 1 {
		t.Fatalf("store saw %d type changes", len(fd.typeCalls))
	}
	if got, want := fd.typeCalls[0], [4]string{"org_1", "srv_1", "general", devActor}; got != want {
		t.Fatalf("delegated %v, want %v (org, server, type, actor)", got, want)
	}
	// Answering with the server is what lets the dialog render the NEW verdict
	// instead of a success toast next to a stale row.
	var srv map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &srv); err != nil {
		t.Fatal(err)
	}
	if srv["id"] != "srv_1" || srv["status"] != store.ServerStatusRunning {
		t.Fatalf("response = %v, want the re-read server", srv)
	}
	if _, ok := srv["incompatibleReasons"]; !ok {
		t.Fatalf("response omits incompatibleReasons, which the dashboard renders: %v", srv)
	}

	// A type outside the catalog never reaches the store.
	rec = postJSON(t, s, "/v1/orgs/org_1/servers/srv_1/type", `{"type":"toaster"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown type → %d, want 400", rec.Code)
	}
	if len(fd.typeCalls) != 1 {
		t.Fatalf("an unknown type was delegated to the store anyway: %v", fd.typeCalls)
	}

	// A store refusal keeps its meaning: re-filing a host whose resources the
	// new type cannot run is a conflict the operator can act on, not a 500.
	fd.typeErr = fmt.Errorf("%w: a storage server cannot host api (App)", store.ErrConflict)
	rec = postJSON(t, s, "/v1/orgs/org_1/servers/srv_1/type", `{"type":"storage"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("blocked re-filing → %d, want 409; body %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "cannot host") {
		t.Fatalf("409 body lost the reason: %s", rec.Body)
	}
}

func TestRenameServerHandler(t *testing.T) {
	fd := &fakeDomain{}
	fs := &fakeStore{servers: []store.Server{{ID: "srv_1", OrgID: "org_1", Name: "hel-general-02"}}}
	s := newTestServerWith(t, fs, fd)

	rec := postJSON(t, s, "/v1/orgs/org_1/servers/srv_1/rename", `{"name":"hel-general-02"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("rename → %d, want 200; body %s", rec.Code, rec.Body)
	}
	if len(fd.renameCalls) != 1 {
		t.Fatalf("store saw %d renames", len(fd.renameCalls))
	}
	if got, want := fd.renameCalls[0], [4]string{"org_1", "srv_1", "hel-general-02", devActor}; got != want {
		t.Fatalf("delegated %v, want %v", got, want)
	}
	// The name is echoed from the re-read row, so a client can render it
	// without guessing that its own input was stored verbatim.
	var srv map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &srv); err != nil {
		t.Fatal(err)
	}
	if srv["name"] != "hel-general-02" {
		t.Fatalf("response name = %v", srv["name"])
	}
}

// Both exits are privileged. A Developer can read the fleet; re-filing a server
// changes what it bills and what may be scheduled on it, and disconnecting it
// is destructive.
func TestServerExitsRequireProjectAdmin(t *testing.T) {
	fs := &fakeStore{serviceTokens: map[string]store.ServicePrincipal{
		"dev-token": {OrgID: "org_1", Name: "reader", Role: store.RoleDeveloper},
	}}
	s := newTestServerWith(t, fs, nil)
	for _, path := range []string{
		"/v1/orgs/org_1/servers/srv_1/type",
		"/v1/orgs/org_1/servers/srv_1/rename",
	} {
		rec := postAsToken(s, "dev-token", path, `{"type":"general","name":"x"}`)
		if rec.Code != http.StatusForbidden {
			t.Errorf("POST %s as Developer → %d, want 403", path, rec.Code)
		}
	}
}

// The register response carries the state the gate just decided, so the agent's
// own logs (and anything replaying the call) can say why a host that installed
// cleanly is not running.
func TestRegisterResponseCarriesCompatibilityState(t *testing.T) {
	fs := &fakeStore{}
	fs.registerResult = &store.RegisterResult{
		AgentToken: "sat_test",
		Server: store.Server{
			ID: "srv_1", OrgID: "org_1", Name: "gpu-box", Type: "gpu",
			Status: store.ServerStatusIncompatible,
			Facts:  json.RawMessage(`{}`),
			IncompatibleReasons: []store.FailedRequirement{{
				ID: store.ReqGPU, Fact: "gpu", Expected: "An NVIDIA GPU with a usable driver.",
				Detected: "none",
				Reason:   "You connected this as a GPU server, but no NVIDIA GPU was detected.",
			}},
		},
	}
	s := newTestServerWith(t, fs, nil)
	rec := postJSON(t, s, "/v1/agent/register", `{"bootstrapToken":"sbt_x","facts":{}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register → %d; body %s", rec.Code, rec.Body)
	}
	var out struct {
		Server store.Server `json:"server"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Server.Status != store.ServerStatusIncompatible {
		t.Fatalf("server.status = %q, want it passed through", out.Server.Status)
	}
	if len(out.Server.IncompatibleReasons) != 1 ||
		out.Server.IncompatibleReasons[0].ID != store.ReqGPU {
		t.Fatalf("server.incompatibleReasons = %+v", out.Server.IncompatibleReasons)
	}
}

// A bootstrap token that is spent or expired is still a 401 — the gate must not
// have turned a credential failure into a compatibility answer.
func TestRegisterStillRejectsBadToken(t *testing.T) {
	fs := &fakeStore{registerErr: store.ErrTokenInvalid}
	s := newTestServerWith(t, fs, nil)
	if rec := postJSON(t, s, "/v1/agent/register", `{"bootstrapToken":"nope"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("register with a spent token → %d, want 401", rec.Code)
	}
}
