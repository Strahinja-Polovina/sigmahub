package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// secretsDomain wraps the shared fakeDomain so a secrets handler test can see
// the config deployments the handler mints. It only overrides the two methods
// the secret write path calls; everything else comes from the embedded fake.
type secretsDomain struct {
	*fakeDomain
	// configDeploys records one entry per CreateConfigDeployments call, so a
	// test can assert on the NUMBER of fleet-wide re-rolls a single operator
	// action causes — the whole point of SIGMA-264.
	configDeploys []string
}

func (d *secretsDomain) AppResourcesForSecretScope(_ context.Context, _, _, _ string) ([]string, error) {
	return []string{"res_1"}, nil
}

func (d *secretsDomain) CreateConfigDeployments(_ context.Context, _ string, _ []string, _, reason string) ([]store.ServerRef, error) {
	d.configDeploys = append(d.configDeploys, reason)
	return nil, nil
}

// TestUpdateSecretValue pins SIGMA-264: rotating a credential is ONE operator
// action, so it must mint exactly one config deployment and keep the secret's
// id (and therefore every ref that names it) intact. Before the fix there was
// no update route at all, so the only rotation path was delete-then-create:
// two config deployments, the first of which re-rolled every dependent app
// WITHOUT the variable.
func TestUpdateSecretValue(t *testing.T) {
	dom := &secretsDomain{fakeDomain: &fakeDomain{}}
	s := New(slog.Default(), fakePinger{}, &fakeStore{}, dom, Options{DevServiceToken: testServiceToken})

	req := httptest.NewRequest("PUT", "/v1/orgs/org_1/secrets/sec_1", strings.NewReader(`{"value":"sk_live_new"}`))
	req.Header.Set("Authorization", "Bearer "+testServiceToken)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("PUT secret value = %d, want 200; body %s", rec.Code, rec.Body)
	}
	var got store.Secret
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v (body %s)", err, rec.Body)
	}
	if got.ID != "sec_1" {
		t.Fatalf("secret id = %q, want it unchanged (sec_1) — refs to it must survive a value change", got.ID)
	}
	if len(dom.configDeploys) != 1 {
		t.Fatalf("config deployments minted = %d (%v), want exactly 1", len(dom.configDeploys), dom.configDeploys)
	}

	// The path this replaces, for contrast: delete-then-create is two rounds of
	// config deployments, and the first of them re-rolls every dependent app
	// with the variable already gone.
	t.Run("delete then create costs two rounds", func(t *testing.T) {
		dom := &secretsDomain{fakeDomain: &fakeDomain{}}
		s := New(slog.Default(), fakePinger{}, &fakeStore{}, dom, Options{DevServiceToken: testServiceToken})
		send := func(method, path, body string) {
			req := httptest.NewRequest(method, path, strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+testServiceToken)
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			if rec.Code >= 300 {
				t.Fatalf("%s %s = %d; body %s", method, path, rec.Code, rec.Body)
			}
		}
		send("DELETE", "/v1/orgs/org_1/secrets/sec_1", "")
		send("POST", "/v1/orgs/org_1/projects/prj_1/secrets",
			`{"name":"STRIPE_SECRET_KEY","value":"sk_live_new","envVar":true}`)
		if len(dom.configDeploys) != 2 {
			t.Fatalf("delete+create minted %d config deployment rounds, want 2 — "+
				"if this changed, the comparison the update path is justified by has moved",
				len(dom.configDeploys))
		}
	})
}
