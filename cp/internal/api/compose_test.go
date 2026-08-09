package api

// The service graph on the wire.
//
// A plain Dockerfile app — most apps — has no compose block, and the dashboard
// loads this endpoint for EVERY app. That path has now been wrong twice in a
// row, in two different ways, and both times the store was right:
//
//  1. The store returned ErrNotFound for "this app has no compose services",
//     which is indistinguishable from "no such resource". The page rendered
//     "the control plane didn't answer for the service graph" on a control
//     plane that had answered correctly and immediately.
//  2. Fixing that to return no services put a nil Go slice on the wire, which
//     marshals to `null` rather than `[]`. The page stopped showing a banner
//     and started throwing `Cannot read properties of null (reading 'length')`
//     — strictly worse, and again invisible to a store-level test, because the
//     store was doing exactly what it had just been fixed to do.
//
// Both bugs live in the gap between "the store's answer" and "the JSON". So
// this test reads the JSON.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// composeStub returns whatever it is given, including nil — which is the whole
// point: nil is what the store returns for an app with no compose block.
type composeStub struct {
	services   []store.ComposeServiceView
	homeServer string
}

func (c composeStub) ComposeServicesForResource(context.Context, string, string) ([]store.ComposeServiceView, string, error) {
	return c.services, c.homeServer, nil
}

func (c composeStub) SetComposePlacements(context.Context, string, string, []store.ComposePlacement, string) ([]string, error) {
	return nil, nil
}

func composeServer(t *testing.T, stub composeStub) *Server {
	t.Helper()
	return New(slog.Default(), fakePinger{}, &fakeStore{}, &fakeDomain{}, Options{
		DevServiceToken: testServiceToken,
		Compose:         stub,
	})
}

func TestAnAppWithNoComposeGraphAnswersAnEmptyListRatherThanNull(t *testing.T) {
	s := composeServer(t, composeStub{services: nil, homeServer: "srv_home"})

	req := httptest.NewRequest(http.MethodGet, "/v1/orgs/org_1/resources/res_1/compose", nil)
	req.Header.Set("Authorization", "Bearer "+testServiceToken)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET compose = %d, want 200; body %s", rec.Code, rec.Body.String())
	}

	// Asserted on the RAW bytes, not on a decoded value: `null` and `[]` both
	// decode into a nil Go slice, so a decoded assertion would pass against the
	// exact payload that crashed the page.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unreadable JSON: %v", err)
	}
	if got := string(raw["services"]); got != "[]" {
		t.Errorf(`services = %s, want []: the dashboard reads .length on this for every app it `+
			`renders, so null is a thrown TypeError on the resource page rather than an empty panel`, got)
	}
	if got := string(raw["homeServerId"]); got != `"srv_home"` {
		t.Errorf("homeServerId = %s, want \"srv_home\"", got)
	}
}

func TestAComposeAppStillReportsItsServices(t *testing.T) {
	// The empty-list guard must not be reachable when there IS a graph — a
	// "fix" that flattened every answer to [] would satisfy the test above.
	s := composeServer(t, composeStub{
		services: []store.ComposeServiceView{
			{Name: "web", Image: "nginx", ServerID: "srv_a"},
			{Name: "worker", Build: ".", DependsOn: []string{"web"}},
		},
		homeServer: "srv_home",
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/orgs/org_1/resources/res_1/compose", nil)
	req.Header.Set("Authorization", "Bearer "+testServiceToken)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	var body struct {
		Services []store.ComposeServiceView `json:"services"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unreadable JSON: %v", err)
	}
	if len(body.Services) != 2 || body.Services[0].Name != "web" || body.Services[1].Name != "worker" {
		t.Fatalf("services = %+v, want the two declared services in order", body.Services)
	}
}
