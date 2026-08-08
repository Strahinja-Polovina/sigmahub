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

// headInspector answers BranchHead with a scripted sha, and records what it was
// asked for so a test can prove the right branch was resolved.
type headInspector struct {
	sha        string
	err        error
	askedRepo  string
	askedBranc string
}

func (h *headInspector) Inspect(context.Context, string, string) (gitdetect.Detected, error) {
	return gitdetect.Detected{}, errors.New("inspect not scripted")
}

func (h *headInspector) BranchHead(_ context.Context, repo, branch, _ string) (string, error) {
	h.askedRepo, h.askedBranc = repo, branch
	return h.sha, h.err
}

func (h *headInspector) RegisterPushWebhook(context.Context, string, string, string, string) error {
	return nil
}

func headDeployServer(t *testing.T, fd *fakeDomain, fg *fakeGit, insp RepoInspector) (*Server, *recReconcile) {
	t.Helper()
	rec := &recReconcile{}
	return New(slog.Default(), fakePinger{}, &fakeStore{}, fd, Options{
		DevServiceToken: testServiceToken,
		Reconcile:       rec,
		Git:             fg,
		Inspector:       insp,
	}), rec
}

func postDeploy(t *testing.T, s *Server) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/orgs/org_1/resources/res_1/deploy", strings.NewReader(""))
	req.Header.Set("Authorization", "Bearer "+testServiceToken)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

// TestDeployButtonShipsRepoHeadOnAFreshResource is the cold start (SIGMA-177).
//
// Every path that could create a deployment read a PREVIOUS one — redeploy
// copied its git coordinates, rollback reused its image, promote replayed the
// last pushed sha — so a resource created minutes ago, with the repo connected
// and a branch mapped, had no way to reach its first build except a git push.
// The Deploy button resolved to "nothing to redeploy" and then to a DSD
// re-apply, which re-runs ops for an app that has no image yet: nothing ships.
//
// With the repo known, the button now resolves the mapped branch's head and
// deploys that commit.
func TestDeployButtonShipsRepoHeadOnAFreshResource(t *testing.T) {
	fd := &fakeDomain{noDeployHistory: true}
	fg := &fakeGit{headOrigin: store.HeadDeployOrigin{
		MapID: "gbm_1", ConnectionID: "gcn_1", RepoFullName: "acme/shop",
		Branch: "main", Ref: "refs/heads/main", EnvironmentID: "env_1", ServerID: "srv_1",
	}}
	insp := &headInspector{sha: "cafebabedeadbeef"}
	s, rec := headDeployServer(t, fd, fg, insp)

	got := postDeploy(t, s)
	if got.Code != http.StatusCreated {
		t.Fatalf("deploy: status=%d body=%s", got.Code, got.Body.String())
	}
	var dep map[string]any
	if err := json.Unmarshal(got.Body.Bytes(), &dep); err != nil {
		t.Fatal(err)
	}
	if dep["gitSha"] != "cafebabedeadbeef" {
		t.Fatalf("deployed sha = %v, want the branch head", dep["gitSha"])
	}
	if fg.headSHA != "cafebabedeadbeef" {
		t.Fatalf("store was handed sha %q", fg.headSHA)
	}
	if insp.askedRepo != "acme/shop" || insp.askedBranc != "main" {
		t.Fatalf("resolved %s#%s, want acme/shop#main", insp.askedRepo, insp.askedBranc)
	}
	if fd.reapplied {
		t.Error("a resource with a connected repo must deploy its code, not fall through to a DSD re-apply")
	}
	if len(rec.calls) != 1 || rec.calls[0] != [2]string{"org_1", "srv_1"} {
		t.Fatalf("reconcile calls = %v, want the deploy target re-rendered once", rec.calls)
	}
}

// A resource that deploys from no repo — a database, object storage, a
// registry-image app — must still reach the force-re-apply path. "Redeploy did
// nothing" stays unreachable.
func TestDeployFallsBackToReapplyWithoutARepo(t *testing.T) {
	fd := &fakeDomain{noDeployHistory: true}
	fg := &fakeGit{} // no origin → ErrNotFound
	s, _ := headDeployServer(t, fd, fg, &headInspector{sha: "unused"})

	got := postDeploy(t, s)
	if got.Code != http.StatusCreated {
		t.Fatalf("deploy: status=%d body=%s", got.Code, got.Body.String())
	}
	if !fd.reapplied {
		t.Fatal("a resource with no repo must fall through to a forced re-apply")
	}
	var body map[string]string
	_ = json.Unmarshal(got.Body.Bytes(), &body)
	if body["trigger"] != "reapply" {
		t.Fatalf("response = %v, want the reapply shape", body)
	}
}

// An unreachable provider is not a failed request: the button still does the
// most it can, which is the re-apply. A 500 here would be a regression — the
// resource may well be one a re-apply fixes.
func TestDeployFallsBackWhenTheProviderIsUnreachable(t *testing.T) {
	fd := &fakeDomain{noDeployHistory: true}
	fg := &fakeGit{headOrigin: store.HeadDeployOrigin{
		MapID: "gbm_1", RepoFullName: "acme/shop", Branch: "main", ServerID: "srv_1",
	}}
	s, _ := headDeployServer(t, fd, fg, &headInspector{err: errors.New("502 from github")})

	got := postDeploy(t, s)
	if got.Code != http.StatusCreated {
		t.Fatalf("deploy: status=%d body=%s", got.Code, got.Body.String())
	}
	if !fd.reapplied {
		t.Fatal("an unreachable provider must fall through, not fail the request")
	}
}

// A resource WITH history keeps replaying its last deployment: the head lookup
// is the cold-start path only, and must not silently change what "redeploy"
// means for an app that is already running a known commit.
func TestDeployWithHistoryStillReplaysTheLastDeployment(t *testing.T) {
	fd := &fakeDomain{}
	fg := &fakeGit{headOrigin: store.HeadDeployOrigin{
		MapID: "gbm_1", RepoFullName: "acme/shop", Branch: "main", ServerID: "srv_1",
	}}
	insp := &headInspector{sha: "newhead"}
	s, _ := headDeployServer(t, fd, fg, insp)

	got := postDeploy(t, s)
	if got.Code != http.StatusCreated {
		t.Fatalf("deploy: status=%d", got.Code)
	}
	if insp.askedRepo != "" {
		t.Error("a resource with deployment history must not resolve a new head")
	}
	if fg.headSHA != "" {
		t.Errorf("head deployment created for a resource with history (sha %q)", fg.headSHA)
	}
}
