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

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/gitdetect"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// recordingInspector captures the token detect/connect used to read the repo.
type recordingInspector struct {
	det       gitdetect.Detected
	lastToken string
}

func (r *recordingInspector) Inspect(_ context.Context, _, token string) (gitdetect.Detected, error) {
	r.lastToken = token
	return r.det, nil
}

func (r *recordingInspector) BranchHead(_ context.Context, _, _, token string) (string, error) {
	r.lastToken = token
	return "", errors.New("branch head not scripted")
}

func (r *recordingInspector) RegisterPushWebhook(context.Context, string, string, string, string) error {
	return errors.New("webhook registration not scripted")
}

// fakeTokenSource is a scripted InstallationTokenSource.
type fakeTokenSource struct {
	err   error
	calls int
}

func (f *fakeTokenSource) InstallationToken(_ context.Context, installationID string) (string, error) {
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	return "ghs_minted:" + installationID, nil
}

func gitAppServer(insp *recordingInspector, src InstallationTokenSource, slug string) *Server {
	return New(slog.Default(), fakePinger{}, &fakeStore{}, &fakeDomain{}, Options{
		Git:                &fakeGit{},
		Inspector:          insp,
		InstallationTokens: src,
		GitHubAppSlug:      slug,
		DevServiceToken:    testServiceToken,
	})
}

func postJSON(t *testing.T, s *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testServiceToken)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// TestGitDetectMintsInstallationToken proves SIGMA-55's read path: a detect
// request that references an App installation (and pastes no token) reads the
// repo with a minted installation token; an explicit token always wins; a
// minting failure degrades to an unauthenticated read instead of failing the
// request outright.
func TestGitDetectMintsInstallationToken(t *testing.T) {
	insp := &recordingInspector{det: gitdetect.Detected{Deployable: true}}
	src := &fakeTokenSource{}
	s := gitAppServer(insp, src, "sigmahub")

	if rec := postJSON(t, s, "/v1/orgs/org_1/git/detect", `{"repoFullName":"o/r","installationId":"42"}`); rec.Code != http.StatusOK {
		t.Fatalf("detect: status = %d, body %s", rec.Code, rec.Body)
	}
	if insp.lastToken != "ghs_minted:42" {
		t.Fatalf("inspector token = %q, want minted installation token", insp.lastToken)
	}

	// A pasted token wins over the installation.
	if rec := postJSON(t, s, "/v1/orgs/org_1/git/detect", `{"repoFullName":"o/r","installationId":"42","token":"ghp_pat"}`); rec.Code != http.StatusOK {
		t.Fatalf("detect with token: status = %d", rec.Code)
	}
	if insp.lastToken != "ghp_pat" {
		t.Fatalf("inspector token = %q, want the pasted PAT", insp.lastToken)
	}

	// Minting failure → unauthenticated read (public repos still detect).
	src.err = errors.New("installation revoked")
	if rec := postJSON(t, s, "/v1/orgs/org_1/git/detect", `{"repoFullName":"o/r","installationId":"42"}`); rec.Code != http.StatusOK {
		t.Fatalf("detect with failed mint: status = %d", rec.Code)
	}
	if insp.lastToken != "" {
		t.Fatalf("inspector token = %q, want empty after mint failure", insp.lastToken)
	}
}

// TestGitInstallationOrgBinding is the SIGMA-87 guard: a detect that references
// an installation bound to ANOTHER org is rejected before any token is minted;
// a first-use installation is claimed for the acting org.
func TestGitInstallationOrgBinding(t *testing.T) {
	insp := &recordingInspector{det: gitdetect.Detected{Deployable: true}}
	src := &fakeTokenSource{}

	// Cross-org installation → the store's claim returns ErrNotFound (opaque 404);
	// no token is minted.
	sCross := New(slog.Default(), fakePinger{}, &fakeStore{}, &fakeDomain{}, Options{
		Git: &fakeGit{claimErr: store.ErrNotFound}, Inspector: insp,
		InstallationTokens: src, DevServiceToken: testServiceToken,
	})
	if rec := postJSON(t, sCross, "/v1/orgs/org_1/git/detect", `{"repoFullName":"o/r","installationId":"999"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-org detect: status = %d, want 404; body %s", rec.Code, rec.Body)
	}
	if src.calls != 0 {
		t.Fatalf("no token should be minted for a cross-org installation, got %d calls", src.calls)
	}

	// First-use installation → claimed for the acting org, token minted.
	fgOK := &fakeGit{}
	sOK := New(slog.Default(), fakePinger{}, &fakeStore{}, &fakeDomain{}, Options{
		Git: fgOK, Inspector: insp, InstallationTokens: src, DevServiceToken: testServiceToken,
	})
	if rec := postJSON(t, sOK, "/v1/orgs/org_1/git/detect", `{"repoFullName":"o/r","installationId":"42"}`); rec.Code != http.StatusOK {
		t.Fatalf("first-use detect: status = %d, body %s", rec.Code, rec.Body)
	}
	if fgOK.claimedInstallation != "42" {
		t.Fatalf("installation not claimed for the acting org, got %q", fgOK.claimedInstallation)
	}
}

// TestGitAppInfo proves the dashboard metadata endpoint reflects configuration.
func TestGitAppInfo(t *testing.T) {
	get := func(s *Server) (int, map[string]any) {
		req := httptest.NewRequest("GET", "/v1/orgs/org_1/git/app", nil)
		req.Header.Set("Authorization", "Bearer "+testServiceToken)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	code, out := get(gitAppServer(&recordingInspector{}, &fakeTokenSource{}, "sigmahub"))
	if code != http.StatusOK || out["enabled"] != true || out["slug"] != "sigmahub" {
		t.Fatalf("configured app info = %d %v", code, out)
	}
	code, out = get(gitAppServer(&recordingInspector{}, nil, ""))
	if code != http.StatusOK || out["enabled"] != false || out["slug"] != "" {
		t.Fatalf("unconfigured app info = %d %v", code, out)
	}
}

// TestSetInstallation proves the post-install callback route links the
// installation onto the connection.
func TestSetInstallation(t *testing.T) {
	fg := &fakeGit{}
	s := New(slog.Default(), fakePinger{}, &fakeStore{}, &fakeDomain{}, Options{
		Git:             fg,
		DevServiceToken: testServiceToken,
	})

	rec := postJSON(t, s, "/v1/orgs/org_1/git/connections/gcn_1/installation", `{"installationId":"42"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("link: status = %d, body %s", rec.Code, rec.Body)
	}
	if fg.linkedConn != "gcn_1" || fg.linkedInstallation != "42" {
		t.Fatalf("linked %q/%q, want gcn_1/42", fg.linkedConn, fg.linkedInstallation)
	}

	// The fake mirrors the store's validation: an empty id is ErrInvalid → 422.
	if rec := postJSON(t, s, "/v1/orgs/org_1/git/connections/gcn_1/installation", `{"installationId":""}`); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("empty installation id: status = %d, want 422", rec.Code)
	}
}
