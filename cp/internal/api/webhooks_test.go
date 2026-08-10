package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/gitdetect"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

const testWebhookSecret = "topsecret"

// fakeGit records the last event and returns a scripted outcome.
type fakeGit struct {
	lastEvent store.GitWebhookEvent
	outcome   store.WebhookOutcome
	err       error
	// linkedConn/linkedInstallation record the last SetConnectionInstallation
	// call (SIGMA-55 handler tests).
	linkedConn         string
	linkedInstallation string
	// claimErr scripts a cross-org rejection; claimedInstallation records the
	// last ClaimInstallation call (SIGMA-87 handler tests).
	claimErr            error
	claimedInstallation string
	// headOrigin scripts the repo+branch a resource deploys from; origin (zero
	// value) means "deploys from no repo" and drives the fall-through. headSHA
	// records the commit the head-deploy path created (SIGMA-177).
	headOrigin    store.HeadDeployOrigin
	headOriginErr error
	headSHA       string
	// recordedHead is the sha handed to RecordBranchHead at map time.
	recordedHead string
}

func (f *fakeGit) HandleGitWebhook(_ context.Context, ev store.GitWebhookEvent) (store.WebhookOutcome, error) {
	f.lastEvent = ev
	return f.outcome, f.err
}
func (f *fakeGit) CreateGitConnection(_ context.Context, orgID string, in store.CreateGitConnectionInput, actor string) (store.GitConnection, error) {
	return store.GitConnection{ID: "gcn_1", OrgID: orgID, ProjectID: in.ProjectID, RepoFullName: in.RepoFullName, CreatedBy: actor}, nil
}
func (f *fakeGit) ListGitConnections(context.Context, string, string) ([]store.GitConnection, error) {
	return []store.GitConnection{}, nil
}
func (f *fakeGit) GetGitConnection(context.Context, string, string) (store.GitConnection, error) {
	return store.GitConnection{}, store.ErrNotFound
}
func (f *fakeGit) DeleteGitConnection(context.Context, string, string, string) error { return nil }
func (f *fakeGit) SetBranchMap(_ context.Context, _, connID, branch, envID, policy, _, _ string) (store.BranchMap, error) {
	return store.BranchMap{ID: "gbm_1", ConnectionID: connID, Branch: branch, EnvironmentID: envID, Policy: policy}, nil
}
func (f *fakeGit) ListBranchMaps(context.Context, string, string) ([]store.BranchMap, error) {
	return []store.BranchMap{}, nil
}
func (f *fakeGit) DeleteBranchMap(context.Context, string, string, string) error { return nil }
func (f *fakeGit) PromoteBranch(context.Context, string, string, string) (store.DeployRequest, error) {
	return store.DeployRequest{ID: "dpr_1", Kind: "deploy", Status: "queued"}, nil
}
func (f *fakeGit) ListDeployRequests(context.Context, string, string, int) ([]store.DeployRequest, error) {
	return []store.DeployRequest{}, nil
}
func (f *fakeGit) SetConnectionPreviews(context.Context, string, string, bool, string, string) error {
	return nil
}
func (f *fakeGit) ListPreviewEnvironments(context.Context, string, string) ([]store.PreviewEnvironment, error) {
	return []store.PreviewEnvironment{}, nil
}
func (f *fakeGit) SetConnectionInstallation(_ context.Context, _, connID, installationID, _ string) error {
	if installationID == "" {
		return store.ErrInvalid{Msg: "installationId must be a numeric GitHub installation id"}
	}
	f.linkedConn, f.linkedInstallation = connID, installationID
	return nil
}
func (f *fakeGit) ClaimInstallation(_ context.Context, _, installationID string) error {
	if f.claimErr != nil {
		return f.claimErr
	}
	f.claimedInstallation = installationID
	return nil
}
func (f *fakeGit) GitTokenForRepo(_ context.Context, _, _ string) (string, error) {
	return "", store.ErrNotFound
}
func (f *fakeGit) EnqueueBranchDeploy(_ context.Context, _, mapID, ref, sha, _ string) (store.DeployRequest, error) {
	return store.DeployRequest{ID: "dr_test", Ref: ref, SHA: sha}, nil
}
func (f *fakeGit) RecordBranchHead(_ context.Context, _, _, _, sha string) error {
	f.recordedHead = sha
	return nil
}
func (f *fakeGit) HeadDeployOriginForResource(context.Context, string, string) (store.HeadDeployOrigin, error) {
	if f.headOriginErr != nil {
		return store.HeadDeployOrigin{}, f.headOriginErr
	}
	if f.headOrigin.RepoFullName == "" {
		return store.HeadDeployOrigin{}, store.ErrNotFound
	}
	return f.headOrigin, nil
}
func (f *fakeGit) CreateHeadDeployment(_ context.Context, _, resourceID, _ string, o store.HeadDeployOrigin, sha string) (store.Deployment, string, error) {
	f.headSHA = sha
	return store.Deployment{ID: "dep_head", ResourceID: resourceID, Trigger: "manual", GitSHA: sha, GitRef: o.Ref, Status: "queued"}, o.ServerID, nil
}

// fakeInspector returns a scripted detection.
type fakeInspector struct {
	det gitdetect.Detected
	err error
}

func (f fakeInspector) Inspect(context.Context, string, string) (gitdetect.Detected, error) {
	return f.det, f.err
}

func (f fakeInspector) BranchHead(context.Context, string, string, string) (string, error) {
	return "", errors.New("branch head not scripted")
}

func (f fakeInspector) RegisterPushWebhook(context.Context, string, string, string, string) error {
	return errors.New("webhook registration not scripted")
}

func sign(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func webhookServer(t *testing.T, fg *fakeGit) *Server {
	t.Helper()
	return New(slog.Default(), fakePinger{}, &fakeStore{}, &fakeDomain{}, Options{
		Git:                 fg,
		GitHubWebhookSecret: testWebhookSecret,
	})
}

func postWebhook(s *Server, event, delivery, sig, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/v1/webhooks/github", strings.NewReader(body))
	if event != "" {
		req.Header.Set("X-GitHub-Event", event)
	}
	if delivery != "" {
		req.Header.Set("X-GitHub-Delivery", delivery)
	}
	if sig != "" {
		req.Header.Set("X-Hub-Signature-256", sig)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestWebhookForgedSignatureRejected(t *testing.T) {
	fg := &fakeGit{}
	s := webhookServer(t, fg)
	body := `{"ref":"refs/heads/main","after":"abc","repository":{"full_name":"o/r"}}`
	// Wrong secret → forged signature.
	rec := postWebhook(s, "push", "d1", sign("wrong-secret", body), body)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("forged signature: status = %d, want 401", rec.Code)
	}
	if fg.lastEvent.DeliveryID != "" {
		t.Error("handler must reject before touching the store")
	}
}

func TestWebhookMissingSignatureRejected(t *testing.T) {
	s := webhookServer(t, &fakeGit{})
	body := `{}`
	rec := postWebhook(s, "push", "d1", "", body)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing signature: status = %d, want 401", rec.Code)
	}
}

func TestWebhookNotConfigured(t *testing.T) {
	// No secret configured → 503, never processes.
	s := New(slog.Default(), fakePinger{}, &fakeStore{}, &fakeDomain{}, Options{Git: &fakeGit{}})
	body := `{}`
	rec := postWebhook(s, "push", "d1", sign(testWebhookSecret, body), body)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured: status = %d, want 503", rec.Code)
	}
}

func TestWebhookPing(t *testing.T) {
	s := webhookServer(t, &fakeGit{})
	body := `{"zen":"hi"}`
	rec := postWebhook(s, "ping", "d1", sign(testWebhookSecret, body), body)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "pong") {
		t.Fatalf("ping: status=%d body=%s", rec.Code, rec.Body)
	}
}

func TestWebhookPushParsed(t *testing.T) {
	fg := &fakeGit{outcome: store.WebhookOutcome{
		Connection: &store.GitConnection{ID: "gcn_1"},
		Enqueued:   &store.DeployRequest{ID: "dpr_9"},
	}}
	s := webhookServer(t, fg)
	body := `{"ref":"refs/heads/main","after":"deadbeef","deleted":false,"repository":{"full_name":"Owner/Repo"}}`
	rec := postWebhook(s, "push", "d-push", sign(testWebhookSecret, body), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("push: status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	ev := fg.lastEvent
	if ev.EventType != "push" || ev.Ref != "refs/heads/main" || ev.SHA != "deadbeef" || ev.RepoFullName != "Owner/Repo" {
		t.Fatalf("parsed event wrong: %+v", ev)
	}
	if ev.Deleted {
		t.Error("non-delete push must not be flagged Deleted")
	}
	if !strings.Contains(rec.Body.String(), "dpr_9") {
		t.Errorf("response should carry the enqueued id; body %s", rec.Body)
	}
}

func TestWebhookBranchDeleteFlagged(t *testing.T) {
	fg := &fakeGit{outcome: store.WebhookOutcome{Connection: &store.GitConnection{ID: "gcn_1"}}}
	s := webhookServer(t, fg)
	// after = zero sha marks a delete even without the deleted flag.
	body := `{"ref":"refs/heads/gone","after":"0000000000000000000000000000000000000000","repository":{"full_name":"o/r"}}`
	rec := postWebhook(s, "push", "d-del", sign(testWebhookSecret, body), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !fg.lastEvent.Deleted {
		t.Error("zero-sha push must set Deleted")
	}
}

func TestWebhookPullRequestParsed(t *testing.T) {
	fg := &fakeGit{outcome: store.WebhookOutcome{
		Connection: &store.GitConnection{ID: "gcn_1"},
		PRHook:     &store.DeployRequest{ID: "dpr_pr", Kind: "pr_hook"},
	}}
	s := webhookServer(t, fg)
	body := `{"action":"opened","pull_request":{"head":{"ref":"feature","sha":"cafe"}},"repository":{"full_name":"o/r"}}`
	rec := postWebhook(s, "pull_request", "d-pr", sign(testWebhookSecret, body), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("pr: status = %d; body %s", rec.Code, rec.Body)
	}
	ev := fg.lastEvent
	if ev.EventType != "pull_request" || ev.Action != "opened" || ev.Branch != "feature" || ev.SHA != "cafe" {
		t.Fatalf("parsed PR event wrong: %+v", ev)
	}
}

func TestWebhookMissingHeaders(t *testing.T) {
	s := webhookServer(t, &fakeGit{})
	body := `{}`
	// Valid signature but no event header.
	rec := postWebhook(s, "", "d1", sign(testWebhookSecret, body), body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing event header: status = %d, want 400", rec.Code)
	}
}

func TestValidGitHubSignatureUnit(t *testing.T) {
	body := []byte("payload")
	good := sign(testWebhookSecret, "payload")
	if !validGitHubSignature(testWebhookSecret, body, good) {
		t.Error("valid signature rejected")
	}
	if validGitHubSignature(testWebhookSecret, body, "sha256=deadbeef") {
		t.Error("wrong digest accepted")
	}
	if validGitHubSignature(testWebhookSecret, body, "") {
		t.Error("empty header accepted")
	}
	if validGitHubSignature(testWebhookSecret, body, "sha1=abcd") {
		t.Error("non-sha256 scheme accepted")
	}
	if validGitHubSignature("", body, good) {
		t.Error("empty secret must never validate")
	}
}

// TestGitConnectDeployabilityGate proves an undeployable repo is refused 422 by
// the connect endpoint when detection is available.
func TestGitConnectDeployabilityGate(t *testing.T) {
	fg := &fakeGit{}
	s := New(slog.Default(), fakePinger{}, &fakeStore{}, &fakeDomain{}, Options{
		Git:             fg,
		Inspector:       fakeInspector{det: gitdetect.Detected{Deployable: false, Reason: "no Dockerfile or Compose file found"}},
		DevServiceToken: testServiceToken,
	})
	body := `{"projectId":"prj_1","repoFullName":"o/r"}`
	req := httptest.NewRequest("POST", "/v1/orgs/org_1/git/connections", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testServiceToken)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("undeployable connect: status = %d, want 422; body %s", rec.Code, rec.Body)
	}
	var resp struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !strings.Contains(resp.Error, "Dockerfile") {
		t.Errorf("rejection should be actionable, got %q", resp.Error)
	}
}

func TestGitConnectDeployableProceeds(t *testing.T) {
	fg := &fakeGit{}
	s := New(slog.Default(), fakePinger{}, &fakeStore{}, &fakeDomain{}, Options{
		Git:             fg,
		Inspector:       fakeInspector{det: gitdetect.Detected{Deployable: true, HasDockerfile: true}},
		DevServiceToken: testServiceToken,
	})
	body := `{"projectId":"prj_1","repoFullName":"o/r"}`
	req := httptest.NewRequest("POST", "/v1/orgs/org_1/git/connections", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testServiceToken)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("deployable connect: status = %d, want 201; body %s", rec.Code, rec.Body)
	}
}
