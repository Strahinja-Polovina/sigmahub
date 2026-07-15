package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

func signActor(t *testing.T, name, role, bearer string) (string, string) {
	t.Helper()
	payload, _ := json.Marshal(map[string]string{"name": name, "role": role})
	b64 := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(bearer))
	mac.Write([]byte(b64))
	return b64, hex.EncodeToString(mac.Sum(nil))
}

// TestActorIdentity pins the P1-1 contract: the signed actor header narrows
// the token's role per user (Developer actor on an Org Admin token cannot
// mutate), never widens it, and a forged signature is rejected outright.
func TestActorIdentity(t *testing.T) {
	s := newTestServer(t, &fakeStore{serviceTokens: map[string]store.ServicePrincipal{
		"sst_admin": {ID: "st_1", OrgID: "org_1", Name: "web", Role: store.RoleOrgAdmin},
		"sst_dev":   {ID: "st_2", OrgID: "org_1", Name: "ro", Role: store.RoleDeveloper},
	}})

	post := func(token, actorB64, sig string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/v1/orgs/org_1/projects", strings.NewReader(`{"name":"api"}`))
		req.Header.Set("Authorization", "Bearer "+token)
		if actorB64 != "" {
			req.Header.Set("X-Sigmahub-Actor", actorB64)
			req.Header.Set("X-Sigmahub-Actor-Signature", sig)
		}
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec
	}

	t.Run("no actor header keeps token role", func(t *testing.T) {
		if rec := post("sst_admin", "", ""); rec.Code != 201 {
			t.Fatalf("status = %d, want 201; body %s", rec.Code, rec.Body)
		}
	})

	t.Run("developer actor narrows an admin token", func(t *testing.T) {
		b64, sig := signActor(t, "dev.user", "Developer", "sst_admin")
		if rec := post("sst_admin", b64, sig); rec.Code != 403 {
			t.Fatalf("status = %d, want 403; body %s", rec.Code, rec.Body)
		}
	})

	t.Run("admin actor cannot widen a developer token", func(t *testing.T) {
		b64, sig := signActor(t, "admin.user", "Org Admin", "sst_dev")
		if rec := post("sst_dev", b64, sig); rec.Code != 403 {
			t.Fatalf("status = %d, want 403; body %s", rec.Code, rec.Body)
		}
	})

	t.Run("project admin actor may mutate", func(t *testing.T) {
		b64, sig := signActor(t, "pa.user", "Project Admin", "sst_admin")
		if rec := post("sst_admin", b64, sig); rec.Code != 201 {
			t.Fatalf("status = %d, want 201; body %s", rec.Code, rec.Body)
		}
	})

	t.Run("forged signature is 401", func(t *testing.T) {
		b64, _ := signActor(t, "evil", "Org Admin", "sst_admin")
		if rec := post("sst_admin", b64, "deadbeef"); rec.Code != 401 {
			t.Fatalf("status = %d, want 401; body %s", rec.Code, rec.Body)
		}
	})
}

// TestIdempotency pins replay semantics: same key+body replays the stored
// response without re-executing; same key with a different body is a 409.
func TestIdempotency(t *testing.T) {
	dom := &fakeDomain{}
	s := New(slog.Default(), fakePinger{}, &fakeStore{}, dom, Options{DevServiceToken: testServiceToken})

	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/v1/orgs/org_1/projects", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+testServiceToken)
		req.Header.Set("Idempotency-Key", "k1")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec
	}

	first := post(`{"name":"api"}`)
	if first.Code != 201 || dom.createCount != 1 {
		t.Fatalf("first: status=%d createCount=%d", first.Code, dom.createCount)
	}
	replay := post(`{"name":"api"}`)
	if replay.Code != 201 || dom.createCount != 1 {
		t.Fatalf("replay executed the handler again: status=%d createCount=%d", replay.Code, dom.createCount)
	}
	if replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("replay missing Idempotency-Replayed header")
	}
	if first.Body.String() != replay.Body.String() {
		t.Fatalf("replay body differs: %q vs %q", first.Body, replay.Body)
	}
	if conflict := post(`{"name":"other"}`); conflict.Code != http.StatusConflict {
		t.Fatalf("key reuse with different body: status=%d, want 409", conflict.Code)
	}
}

// TestProvisioning pins the org-provisioning gate.
func TestProvisioning(t *testing.T) {
	s := newTestServer(t, nil)
	post := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/v1/orgs", strings.NewReader(`{"orgId":"org_9"}`))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec
	}
	if rec := post(""); rec.Code != 401 {
		t.Fatalf("no token: %d, want 401", rec.Code)
	}
	if rec := post("nope"); rec.Code != 401 {
		t.Fatalf("wrong token: %d, want 401", rec.Code)
	}
	rec := post(testProvisionToken)
	if rec.Code != 201 {
		t.Fatalf("provision token: %d, want 201; body %s", rec.Code, rec.Body)
	}
	var res struct {
		Token string `json:"token"`
		Role  string `json:"role"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || res.Token == "" || res.Role != "Org Admin" {
		t.Fatalf("bad provision response: %s (err %v)", rec.Body, err)
	}
}
