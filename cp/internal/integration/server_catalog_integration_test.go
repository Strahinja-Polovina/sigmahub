package integration

// Onboarding every canonical server type, over HTTP, against a real database.
//
// The store's type check, the API's type check and the servers table's column
// were three separate opinions. The API held its own four-name list, so three
// types that the store accepted and the dashboard advertised were rejected at
// the edge with a bare 400 — a defect no unit test could see, because the fake
// store in the handler tests never disagreed with the real one. This drives the
// actual HTTP handler into the actual Postgres for each type in the catalog.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/api"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// catalogAPI is the smallest server that can serve the onboarding routes.
func catalogAPI(t *testing.T, st *store.Store) (*httptest.Server, string) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := api.New(log, st, st, st, api.Options{DevServiceToken: "dev"})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, "dev"
}

func postAs(t *testing.T, ts *httptest.Server, token, path string, body any) (int, map[string]any) {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("POST", ts.URL+path, bytes.NewReader(buf))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestProvisionAcceptsEveryCanonicalServerType(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	ts, token := catalogAPI(t, st)
	orgID := "org_catalog"

	for _, typ := range store.ServerTypes() {
		t.Run(typ, func(t *testing.T) {
			code, body := postAs(t, ts, token, "/v1/orgs/"+orgID+"/servers/provision", map[string]any{
				"name": "host-" + typ, "type": typ, "distro": "ubuntu-24.04",
			})
			if code != http.StatusCreated {
				t.Fatalf("provision %q → %d: %v", typ, code, body)
			}
			serverID, _ := body["serverId"].(string)
			if serverID == "" {
				t.Fatalf("provision %q returned no server id: %v", typ, body)
			}
			// The type must survive to the row: an accepted request that stored
			// something else would bill and schedule as the wrong thing.
			srv, err := st.GetServer(ctx, orgID, serverID)
			if err != nil {
				t.Fatal(err)
			}
			if srv.Type != typ {
				t.Fatalf("stored type = %q, want %q", srv.Type, typ)
			}
			// The manual/NAT path is a second entry point with its own check.
			code, body = postAs(t, ts, token, "/v1/orgs/"+orgID+"/bootstrap-tokens", map[string]any{
				"name": "manual-" + typ, "type": typ,
			})
			if code != http.StatusCreated {
				t.Fatalf("bootstrap-token %q → %d: %v", typ, code, body)
			}
		})
	}

	// And the fleet bills at the catalog's weights, with no type falling
	// through to the default because a second table forgot about it.
	for _, typ := range store.ServerTypes() {
		connectTypedServer(t, st, orgID+"_billing", "b-"+typ, typ)
	}
	lines, servers, units, err := st.ConnectedServerUnits(ctx, orgID+"_billing")
	if err != nil {
		t.Fatal(err)
	}
	if servers != len(store.ServerTypes()) {
		t.Fatalf("connected = %d, want %d", servers, len(store.ServerTypes()))
	}
	want := 0
	for _, typ := range store.ServerTypes() {
		want += store.ServerUnitWeight(typ)
	}
	if units != want {
		t.Fatalf("units = %d, want %d (lines %+v)", units, want, lines)
	}
	for _, line := range lines {
		if line.Weight != store.ServerUnitWeight(line.Type) {
			t.Fatalf("%s billed at weight %d, catalog says %d", line.Type, line.Weight,
				store.ServerUnitWeight(line.Type))
		}
	}
}

// A type outside the catalog must be refused at the edge, with a message that
// names the alternatives — the old bare "invalid server type" left an operator
// guessing at a list they could not see.
func TestProvisionRejectsUnknownServerType(t *testing.T) {
	st, _ := testStore(t)
	ts, token := catalogAPI(t, st)

	code, body := postAs(t, ts, token, "/v1/orgs/org_catalog/servers/provision", map[string]any{
		"name": "host", "type": "toaster",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("unknown type → %d: %v", code, body)
	}
	msg, _ := body["error"].(string)
	for _, typ := range store.ServerTypes() {
		if !bytes.Contains([]byte(msg), []byte(typ)) {
			t.Fatalf("error %q does not offer %q as an alternative", msg, typ)
		}
	}
}

// Every kind the catalog knows must be creatable on every server type the
// matrix says can host it — and refused everywhere else. This is the rule the
// deploy wizard renders from, so a disagreement here is a wizard that offers a
// server the API then answers 422 for.
func TestAvailabilityMatrixHoldsAgainstTheDatabase(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_matrix"

	proj, err := st.CreateProject(ctx, orgID, "p", "", "test")
	if err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, orgID, proj.ID, "prod", true, "test")
	if err != nil {
		t.Fatal(err)
	}
	// One attached server per type, so every cell of the matrix is reachable.
	serverOf := map[string]string{}
	for _, typ := range store.ServerTypes() {
		id := connectTypedServer(t, st, orgID, "host-"+typ, typ)
		if err := st.AttachServer(ctx, orgID, env.ID, id, "test"); err != nil {
			t.Fatal(err)
		}
		serverOf[typ] = id
	}

	n := 0
	for _, kind := range store.ResourceKinds() {
		for _, typ := range store.ServerTypes() {
			n++
			name := "r" + strconv.Itoa(n)
			_, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
				EnvironmentID: env.ID, ServerID: serverOf[typ], Name: name, Kind: kind,
			}, "test")
			allowed := store.CanHost(typ, kind)
			if allowed && err != nil {
				t.Errorf("%s on a %s server was refused: %v", kind, typ, err)
			}
			if !allowed && err == nil {
				t.Errorf("%s was created on a %s server, which the matrix forbids", kind, typ)
			}
		}
	}
}
