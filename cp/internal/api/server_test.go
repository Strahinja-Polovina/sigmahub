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

func (f *fakeStore) IssueBootstrapToken(_ context.Context, orgID, name, typ, provider, region, createdBy string, ttl time.Duration) (string, string, time.Time, error) {
	return "sbt_test", "srv_pre", time.Now().Add(ttl), nil
}

func (f *fakeStore) ProvisionServer(_ context.Context, orgID string, in store.ProvisionInput, createdBy string, ttl time.Duration) (store.ProvisionResult, error) {
	return store.ProvisionResult{
		ServerID: "srv_pre", Token: "sbt_test", ExpiresAt: time.Now().Add(ttl),
		BootstrapPubkey: "ssh-ed25519 AAAA sigmahub-bootstrap",
	}, nil
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

func (f *fakeStore) MeshPeers(context.Context, string, string) ([]store.MeshPeer, error) {
	return []store.MeshPeer{}, nil
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
func (f *fakeStore) ResolveSecretsForResource(context.Context, string, string, string, string) ([]store.ResolvedSecret, error) {
	return []store.ResolvedSecret{}, nil
}
func (f *fakeStore) SetDomainCertStatus(context.Context, string, string, string, string, *time.Time, string) error {
	return nil
}
func (f *fakeStore) DeploymentCloneCredential(context.Context, string, string) (string, string, string, error) {
	return "", "", "", nil
}
func (f *fakeStore) AdvanceDeploymentForResource(context.Context, string, string, string, bool, string, int64) error {
	return nil
}
func (f *fakeStore) AdvanceDeploymentService(context.Context, string, string, string, string, bool, string, int64) error {
	return nil
}
func (f *fakeStore) AppendDeployLog(context.Context, string, string, string, string) error {
	return nil
}
func (f *fakeStore) BackupCredentialForRun(context.Context, string, string) (store.BackupCredential, error) {
	return store.BackupCredential{}, store.ErrNotFound
}
func (f *fakeStore) WALTargetsForServer(context.Context, string) ([]store.WALTarget, error) {
	return []store.WALTarget{}, nil
}
func (f *fakeStore) WALCredentialForResource(context.Context, string, string) (store.BackupCredential, error) {
	return store.BackupCredential{}, store.ErrNotFound
}
func (f *fakeStore) SetWALStatus(context.Context, string, string, string, time.Time) error {
	return nil
}
func (f *fakeStore) SetBackupRunResult(context.Context, string, string, bool, string, string, string) error {
	return nil
}
func (f *fakeStore) FailBackupRunFromOpStatus(context.Context, string, string, string) error {
	return nil
}
func (f *fakeStore) S3OpCredentialForOp(context.Context, string, string) (store.S3OpCredential, error) {
	return store.S3OpCredential{}, store.ErrNotFound
}
func (f *fakeStore) MarkS3OpApplied(context.Context, string, string, string) error { return nil }
func (f *fakeStore) MarkS3OpFailed(context.Context, string, string, string) error  { return nil }
func (f *fakeStore) RecordStorageBytes(context.Context, string, string, int64, time.Time) error {
	return nil
}
func (f *fakeStore) FailS3OpFromOpStatus(context.Context, string, string, string) error { return nil }

// fakeDomain implements DomainAPI in-memory for handler tests.
type fakeDomain struct {
	idem map[string]store.IdempotentResponse
	// createCount counts CreateProject executions to prove replay skips them.
	createCount int
}

func (f *fakeDomain) CreateProject(_ context.Context, orgID, name, desc, actor string) (store.Project, error) {
	f.createCount++
	return store.Project{ID: "prj_1", OrgID: orgID, Name: name, Description: desc, CreatedBy: actor}, nil
}
func (f *fakeDomain) ListProjects(context.Context, string) ([]store.Project, error) {
	return []store.Project{}, nil
}
func (f *fakeDomain) GetProject(context.Context, string, string) (store.Project, error) {
	return store.Project{}, store.ErrNotFound
}
func (f *fakeDomain) UpdateProject(_ context.Context, orgID, projectID, name, desc, _ string) (store.Project, error) {
	return store.Project{ID: projectID, OrgID: orgID, Name: name, Description: desc}, nil
}
func (f *fakeDomain) DeleteProject(context.Context, string, string, string) error { return nil }
func (f *fakeDomain) CreateEnvironment(_ context.Context, orgID, projectID, name string, prod bool, _ string) (store.Environment, error) {
	return store.Environment{ID: "env_1", OrgID: orgID, ProjectID: projectID, Name: name, Production: prod}, nil
}
func (f *fakeDomain) ListEnvironments(context.Context, string, string) ([]store.Environment, error) {
	return []store.Environment{}, nil
}
func (f *fakeDomain) DeleteEnvironment(context.Context, string, string, string) error { return nil }
func (f *fakeDomain) AttachServer(context.Context, string, string, string, string) error {
	return nil
}
func (f *fakeDomain) DetachServer(context.Context, string, string, string, string) error {
	return nil
}
func (f *fakeDomain) EnvServerIDs(context.Context, string, string) ([]string, error) {
	return []string{}, nil
}
func (f *fakeDomain) CreateResource(_ context.Context, orgID string, in store.CreateResourceInput, _ string) (store.Resource, error) {
	if store.AllowedServerTypes(in.Kind) == nil {
		return store.Resource{}, store.ErrInvalid{Msg: "unknown resource kind"}
	}
	return store.Resource{ID: "res_1", OrgID: orgID, Name: in.Name, Kind: in.Kind}, nil
}
func (f *fakeDomain) ListResources(context.Context, string, string) ([]store.Resource, error) {
	return []store.Resource{}, nil
}
func (f *fakeDomain) DeleteResource(context.Context, string, string, string) (string, error) {
	return "", nil
}
func (f *fakeDomain) SetHardeningConfig(context.Context, string, string, bool, bool, []store.PortException, string) error {
	return nil
}

func (f *fakeDomain) SetProxyRole(context.Context, string, string, bool, string) error {
	return nil
}
func (f *fakeDomain) ListAudit(context.Context, string, int) ([]store.AuditEntry, error) {
	return []store.AuditEntry{}, nil
}
func (f *fakeDomain) IdempotencyLookup(_ context.Context, orgID, key string) (store.IdempotentResponse, error) {
	if r, ok := f.idem[orgID+"/"+key]; ok {
		return r, nil
	}
	return store.IdempotentResponse{}, store.ErrNotFound
}
func (f *fakeDomain) IdempotencyClaim(_ context.Context, orgID, key string, reqHash []byte) (bool, store.IdempotentResponse, error) {
	if f.idem == nil {
		f.idem = map[string]store.IdempotentResponse{}
	}
	k := orgID + "/" + key
	if r, ok := f.idem[k]; ok {
		return false, r, nil
	}
	f.idem[k] = store.IdempotentResponse{RequestHash: reqHash} // pending (Done=false)
	return true, store.IdempotentResponse{}, nil
}
func (f *fakeDomain) IdempotencyFinalize(_ context.Context, orgID, key string, statusCode int, response []byte) error {
	k := orgID + "/" + key
	f.idem[k] = store.IdempotentResponse{RequestHash: f.idem[k].RequestHash, StatusCode: statusCode, Response: response, Done: true}
	return nil
}
func (f *fakeDomain) IdempotencyRelease(_ context.Context, orgID, key string) error {
	delete(f.idem, orgID+"/"+key)
	return nil
}
func (f *fakeDomain) IssueServiceToken(_ context.Context, orgID, name string, role store.Role, _ string) (string, store.ServicePrincipal, error) {
	return "sst_provisioned", store.ServicePrincipal{ID: "st_p", OrgID: orgID, Name: name, Role: role}, nil
}
func (f *fakeDomain) IssueConfirmToken(_ context.Context, _, _, _, _, _ string, _ time.Duration) (string, time.Time, error) {
	return "sct_confirm", time.Now().Add(time.Minute), nil
}
func (f *fakeDomain) ConfirmDestructiveOp(_ context.Context, _, _, _, _, _, _ string) (string, error) {
	return "pdo_1", nil
}
func (f *fakeDomain) DeleteServer(_ context.Context, _, _, _ string) error     { return nil }
func (f *fakeDomain) RevokeAgentToken(_ context.Context, _, _, _ string) error { return nil }
func (f *fakeDomain) ListServiceTokens(_ context.Context, _ string) ([]store.ServiceTokenInfo, error) {
	return []store.ServiceTokenInfo{}, nil
}
func (f *fakeDomain) RevokeServiceToken(_ context.Context, _, _, _ string) error { return nil }
func (f *fakeDomain) RotateServiceToken(_ context.Context, _, _, _ string) (string, store.ServicePrincipal, error) {
	return "sst_rotated", store.ServicePrincipal{}, nil
}
func (f *fakeDomain) CreateSecret(_ context.Context, _, _ string, in store.CreateSecretInput) (store.Secret, error) {
	return store.Secret{ID: "sec_1", ProjectID: in.ProjectID, Name: in.Name, EnvVar: in.EnvVar}, nil
}
func (f *fakeDomain) ListSecrets(_ context.Context, _, _, _ string) ([]store.Secret, error) {
	return []store.Secret{}, nil
}
func (f *fakeDomain) RevealSecret(_ context.Context, _, _, _ string) (string, error) {
	return "value", nil
}
func (f *fakeDomain) DeleteSecret(_ context.Context, _, _, _ string) error      { return nil }
func (f *fakeDomain) RotateKEK(_ context.Context, _, _ string) (int, error)     { return 0, nil }
func (f *fakeDomain) RotateDEK(_ context.Context, _, _ string) (string, error)  { return "dek_2", nil }
func (f *fakeDomain) ReencryptSecrets(_ context.Context, _ string) (int, error) { return 0, nil }
func (f *fakeDomain) GetDatabaseInfo(_ context.Context, _, _ string) (store.DatabaseInfo, error) {
	return store.DatabaseInfo{}, store.ErrNotDatabase
}
func (f *fakeDomain) RevealDatabaseConnection(_ context.Context, _, _, _ string) (store.DatabaseConnection, error) {
	return store.DatabaseConnection{}, store.ErrNotDatabase
}
func (f *fakeDomain) GetS3Info(_ context.Context, _, _ string) (store.S3Info, error) {
	return store.S3Info{}, store.ErrNotS3
}
func (f *fakeDomain) RevealS3Connection(_ context.Context, _, _, _ string) (store.S3Connection, error) {
	return store.S3Connection{}, store.ErrNotS3
}
func (f *fakeDomain) ListBuckets(_ context.Context, _, _ string) ([]store.Bucket, error) {
	return []store.Bucket{}, nil
}
func (f *fakeDomain) CreateBucket(_ context.Context, _, _, _, _ string) (store.Bucket, string, error) {
	return store.Bucket{}, "", store.ErrNotS3
}
func (f *fakeDomain) DeleteBucket(_ context.Context, _, _, _, _ string) (string, error) {
	return "", store.ErrNotFound
}
func (f *fakeDomain) SetBucketQuota(_ context.Context, _, _, _ string, _ int64, _ string) (string, error) {
	return "", store.ErrNotFound
}
func (f *fakeDomain) CreateBucketKey(_ context.Context, _, _, _, _ string) (string, string, error) {
	return "", "", store.ErrNotFound
}
func (f *fakeDomain) CreateBackupTarget(_ context.Context, _, _ string, _ store.CreateBackupTargetInput) (store.BackupTarget, error) {
	return store.BackupTarget{ID: "bkt_1"}, nil
}
func (f *fakeDomain) ListBackupTargets(_ context.Context, _ string) ([]store.BackupTarget, error) {
	return []store.BackupTarget{}, nil
}
func (f *fakeDomain) DeleteBackupTarget(_ context.Context, _, _, _ string) error { return nil }
func (f *fakeDomain) UpdateBackupPolicy(_ context.Context, _, _, _ string, _ store.UpdateBackupPolicyInput) (store.BackupPolicy, error) {
	return store.BackupPolicy{}, store.ErrNotDatabase
}
func (f *fakeDomain) ListBackupRuns(_ context.Context, _, _ string, _ int) ([]store.BackupRun, error) {
	return []store.BackupRun{}, nil
}
func (f *fakeDomain) VerifyDays(_ context.Context, _ string, _ int) ([]store.VerifyDay, error) {
	return []store.VerifyDay{}, nil
}
func (f *fakeDomain) CreateRestoreRun(_ context.Context, _, _, _, _ string) (store.BackupRun, error) {
	return store.BackupRun{}, store.ErrNotFound
}
func (f *fakeDomain) CreateRestoreToTimestampRun(_ context.Context, _, _, _ string, _ time.Time, _ string) (store.BackupRun, error) {
	return store.BackupRun{}, store.ErrNotFound
}
func (f *fakeDomain) AttachDomain(_ context.Context, orgID, resourceID, domain, challengeType, _ string) (store.Domain, string, error) {
	return store.Domain{ID: "dom_1", OrgID: orgID, ResourceID: resourceID, Domain: domain, ChallengeType: challengeType, CertStatus: "pending"}, "srv_1", nil
}
func (f *fakeDomain) DetachDomain(_ context.Context, _, _, _ string) (string, error) {
	return "srv_1", nil
}
func (f *fakeDomain) ListDomainsForResource(_ context.Context, _, _ string) ([]store.Domain, error) {
	return []store.Domain{}, nil
}
func (f *fakeDomain) ListDeployments(_ context.Context, _, resourceID string, _ int) ([]store.Deployment, error) {
	return []store.Deployment{}, nil
}
func (f *fakeDomain) RollbackTargets(_ context.Context, _, _ string, _ int) ([]store.Deployment, error) {
	return []store.Deployment{}, nil
}
func (f *fakeDomain) CreateRollback(_ context.Context, orgID, resourceID, targetDeploymentID, _ string) (store.Deployment, string, error) {
	return store.Deployment{ID: "dep_rb", OrgID: orgID, ResourceID: resourceID, Trigger: "rollback", RollbackOf: targetDeploymentID, Status: "queued"}, "srv_1", nil
}
func (f *fakeDomain) CreateManualRedeploy(_ context.Context, orgID, resourceID, _ string) (store.Deployment, string, error) {
	return store.Deployment{ID: "dep_md", OrgID: orgID, ResourceID: resourceID, Trigger: "manual", Status: "queued"}, "srv_1", nil
}
func (f *fakeDomain) GetDeployment(_ context.Context, orgID, deploymentID string) (store.Deployment, error) {
	return store.Deployment{ID: deploymentID, OrgID: orgID, Status: "success"}, nil
}
func (f *fakeDomain) DeployLogsSince(_ context.Context, _ string, _ int64, _ int) ([]store.DeployLog, error) {
	return []store.DeployLog{}, nil
}
func (f *fakeDomain) CreateAlertChannel(_ context.Context, _, actor string, in store.CreateAlertChannelInput) (store.AlertChannel, error) {
	return store.AlertChannel{ID: "alch_1", Kind: in.Kind, Name: in.Name, Enabled: true, Events: store.AlertEvents, CreatedBy: actor}, nil
}
func (f *fakeDomain) ListAlertChannels(context.Context, string) ([]store.AlertChannel, error) {
	return []store.AlertChannel{}, nil
}
func (f *fakeDomain) DeleteAlertChannel(context.Context, string, string, string) error { return nil }
func (f *fakeDomain) SetAlertRules(_ context.Context, _, _ string, events []string, _ string) error {
	return nil
}
func (f *fakeDomain) AlertChannelForSend(_ context.Context, orgID, channelID string) (store.AlertChannelSend, error) {
	return store.AlertChannelSend{ID: channelID, OrgID: orgID, Kind: "webhook", Config: []byte(`{"url":"http://example.invalid"}`)}, nil
}

const (
	testServiceToken   = "test-service-token"
	testProvisionToken = "test-provision-token"
)

func newTestServer(t *testing.T, fs *fakeStore) *Server {
	t.Helper()
	if fs == nil {
		fs = &fakeStore{}
	}
	return New(slog.Default(), fakePinger{}, fs, &fakeDomain{}, Options{
		DevServiceToken: testServiceToken,
		ProvisionToken:  testProvisionToken,
	})
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
			s := New(slog.Default(), fakePinger{err: tc.err}, &fakeStore{}, &fakeDomain{}, Options{DevServiceToken: testServiceToken})
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
	s := New(slog.Default(), fakePinger{}, &fakeStore{}, &fakeDomain{}, Options{})
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

// TestLoggingRecordersExposeFlusher pins SIGMA-133: the middleware wrappers must
// keep the underlying writer's http.Flusher reachable, or the deploy-log SSE
// path returns 500 "streaming unsupported" on every request.
func TestLoggingRecordersExposeFlusher(t *testing.T) {
	if _, ok := any(&statusRecorder{ResponseWriter: httptest.NewRecorder()}).(http.Flusher); !ok {
		t.Error("statusRecorder must expose http.Flusher")
	}
	if _, ok := any(&responseRecorder{ResponseWriter: httptest.NewRecorder()}).(http.Flusher); !ok {
		t.Error("responseRecorder must expose http.Flusher")
	}
}
