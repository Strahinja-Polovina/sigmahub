package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

// fakeStore implements StoreAPI in-memory for handler tests.
type fakeStore struct {
	registerErr   error
	servers       []store.Server
	metrics       []store.MetricPoint
	serviceTokens map[string]store.ServicePrincipal
}

func (f *fakeStore) IssueBootstrapToken(_ context.Context, orgID, name, typ, provider, region, createdBy string, ttl time.Duration) (string, time.Time, error) {
	return "sbt_test", time.Now().Add(ttl), nil
}

func (f *fakeStore) RegisterServer(_ context.Context, tok, name, ver string, facts json.RawMessage, pubkey string) (store.RegisterResult, error) {
	if f.registerErr != nil {
		return store.RegisterResult{}, f.registerErr
	}
	return store.RegisterResult{
		Server:     store.Server{ID: "srv_1", OrgID: "org_1", Name: name, Facts: json.RawMessage(`{}`)},
		AgentToken: "sat_test",
	}, nil
}

func (f *fakeStore) ServerByAgentToken(context.Context, string) (store.Server, error) {
	return store.Server{}, store.ErrNotFound
}

func (f *fakeStore) AuthenticateServiceToken(_ context.Context, tok string) (store.ServicePrincipal, error) {
	if p, ok := f.serviceTokens[tok]; ok {
		return p, nil
	}
	return store.ServicePrincipal{}, store.ErrNotFound
}

func (f *fakeStore) RecordHeartbeat(context.Context, string, store.HeartbeatInput) error {
	return nil
}

func (f *fakeStore) MetricsSince(context.Context, string, string, time.Time) ([]store.MetricPoint, error) {
	return f.metrics, nil
}

func (f *fakeStore) ListServers(context.Context, string) ([]store.Server, error) {
	return f.servers, nil
}

func (f *fakeStore) GetServer(context.Context, string, string) (store.Server, error) {
	return store.Server{}, store.ErrNotFound
}

const testServiceToken = "test-service-token"

func newTestServer(t *testing.T, fs *fakeStore) *Server {
	t.Helper()
	if fs == nil {
		fs = &fakeStore{}
	}
	return New(slog.Default(), fakePinger{}, fs, testServiceToken)
}

func TestHealthz(t *testing.T) {
	s := newTestServer(t, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Fatalf("healthz = %d, want 200", rec.Code)
	}
}

func TestReadyz(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"db up", nil, 200},
		{"db down", errors.New("nope"), 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(slog.Default(), fakePinger{err: tc.err}, &fakeStore{}, testServiceToken)
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
			if rec.Code != tc.want {
				t.Fatalf("readyz = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestServiceAuth(t *testing.T) {
	s := newTestServer(t, &fakeStore{serviceTokens: map[string]store.ServicePrincipal{
		"sst_admin_org1": {ID: "st_1", OrgID: "org_1", Name: "web", Role: store.RoleProjectAdmin},
		"sst_dev_org1":   {ID: "st_2", OrgID: "org_1", Name: "ro", Role: store.RoleDeveloper},
		"sst_admin_org2": {ID: "st_3", OrgID: "org_2", Name: "other", Role: store.RoleOrgAdmin},
	}})
	for _, tc := range []struct {
		name   string
		method string
		path   string
		token  string
		want   int
	}{
		{"no token", "POST", "/v1/orgs/org_1/bootstrap-tokens", "", 401},
		{"unknown token", "POST", "/v1/orgs/org_1/bootstrap-tokens", "nope", 401},
		{"dev static token is wildcard admin", "POST", "/v1/orgs/org_1/bootstrap-tokens", testServiceToken, 201},
		{"org-scoped admin, right org", "POST", "/v1/orgs/org_1/bootstrap-tokens", "sst_admin_org1", 201},
		{"wrong-org token → 403", "POST", "/v1/orgs/org_1/bootstrap-tokens", "sst_admin_org2", 403},
		{"role below required → 403", "POST", "/v1/orgs/org_1/bootstrap-tokens", "sst_dev_org1", 403},
		{"developer can read", "GET", "/v1/orgs/org_1/servers", "sst_dev_org1", 200},
		{"developer cannot read other org", "GET", "/v1/orgs/org_2/servers", "sst_dev_org1", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var req *http.Request
			if tc.method == "POST" {
				req = httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
			} else {
				req = httptest.NewRequest(tc.method, tc.path, nil)
			}
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tc.want, rec.Body)
			}
		})
	}
}

// TestServiceAuthNoDevToken pins the prod posture: with no static dev token
// configured, only real service tokens pass.
func TestServiceAuthNoDevToken(t *testing.T) {
	s := New(slog.Default(), fakePinger{}, &fakeStore{}, "")
	req := httptest.NewRequest("GET", "/v1/orgs/org_1/servers", nil)
	req.Header.Set("Authorization", "Bearer "+testServiceToken)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestRegister(t *testing.T) {
	t.Run("valid token", func(t *testing.T) {
		s := newTestServer(t, nil)
		req := httptest.NewRequest("POST", "/v1/agent/register",
			strings.NewReader(`{"bootstrapToken":"sbt_x","name":"host-1"}`))
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != 201 {
			t.Fatalf("register = %d, want 201; body %s", rec.Code, rec.Body)
		}
		var res struct {
			ServerID   string `json:"serverId"`
			AgentToken string `json:"agentToken"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		if res.ServerID == "" || res.AgentToken == "" {
			t.Fatalf("missing ids in response: %s", rec.Body)
		}
	})

	t.Run("invalid token → 401", func(t *testing.T) {
		s := newTestServer(t, &fakeStore{registerErr: store.ErrTokenInvalid})
		req := httptest.NewRequest("POST", "/v1/agent/register",
			strings.NewReader(`{"bootstrapToken":"sbt_bad"}`))
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Fatalf("register = %d, want 401", rec.Code)
		}
	})

	t.Run("missing token → 400", func(t *testing.T) {
		s := newTestServer(t, nil)
		req := httptest.NewRequest("POST", "/v1/agent/register", strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != 400 {
			t.Fatalf("register = %d, want 400", rec.Code)
		}
	})
}
