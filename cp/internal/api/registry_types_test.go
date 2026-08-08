package api

// The HTTP boundary's server-type check.
//
// This guard belongs in the Go module, not only in the dashboard's vitest run:
// the boundary used to hold a private list of four types while the store knew
// seven, and nothing in `go test ./...` noticed — the disagreement only became
// visible when a user pressed "VPS" in the connect dialog and got a 400.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// Both onboarding routes must accept EVERY canonical type. Not "the connectable
// ones": a boundary that accepts less than the store is precisely the split
// brain SIGMA-198 removes, and `k8s` being unavailable in the connect dialog is
// a UI decision, not an authorization rule.
func TestOnboardingAcceptsEveryCanonicalServerType(t *testing.T) {
	s := newTestServer(t, nil)
	for _, typ := range store.ServerTypes() {
		body := `{"name":"host","type":"` + typ + `"}`
		for _, path := range []string{
			"/v1/orgs/org_1/bootstrap-tokens",
			"/v1/orgs/org_1/servers/provision",
		} {
			rec := postJSON(t, s, path, body)
			if rec.Code != http.StatusCreated {
				t.Errorf("POST %s type=%q → %d, want 201; body %s", path, typ, rec.Code, rec.Body)
			}
		}
	}
}

func TestOnboardingRejectsUnknownServerType(t *testing.T) {
	s := newTestServer(t, nil)
	for _, path := range []string{
		"/v1/orgs/org_1/bootstrap-tokens",
		"/v1/orgs/org_1/servers/provision",
	} {
		rec := postJSON(t, s, path, `{"name":"host","type":"toaster"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("POST %s → %d, want 400; body %s", path, rec.Code, rec.Body)
		}
		var out map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		// The old message was a bare "invalid server type", which told an
		// operator nothing about what to send instead.
		for _, want := range []string{`"toaster"`, "general", "k8s"} {
			if !strings.Contains(out["error"], want) {
				t.Errorf("error %q does not mention %q", out["error"], want)
			}
		}
	}
}

// An absent type still means "general" — the manual/NAT path posts no type.
func TestOnboardingDefaultsToGeneral(t *testing.T) {
	s := newTestServer(t, nil)
	if rec := postJSON(t, s, "/v1/orgs/org_1/bootstrap-tokens", `{"name":"host"}`); rec.Code != http.StatusCreated {
		t.Fatalf("empty type → %d, want 201; body %s", rec.Code, rec.Body)
	}
}

// The rejection sentence is generated from the catalog's distro labels, so
// adding a distro cannot leave a stale prose list behind at either call site.
func TestUnsupportedDistroMessageFollowsTheCatalog(t *testing.T) {
	s := newTestServer(t, nil)
	rec := postJSON(t, s, "/v1/orgs/org_1/servers/provision", `{"name":"host","distro":"centos-7"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unsupported distro → %d, want 422; body %s", rec.Code, rec.Body)
	}
	var out map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out["error"], store.SupportedDistroSentence()) {
		t.Fatalf("error %q does not carry the catalog's distro list %q",
			out["error"], store.SupportedDistroSentence())
	}
	for _, distro := range store.SupportedDistros() {
		rec := postJSON(t, s, "/v1/orgs/org_1/servers/provision",
			`{"name":"host","distro":"`+distro+`"}`)
		if rec.Code != http.StatusCreated {
			t.Errorf("supported distro %q → %d, want 201; body %s", distro, rec.Code, rec.Body)
		}
	}
}
