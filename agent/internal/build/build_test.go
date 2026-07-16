package build

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/agent/internal/dsd"
)

type capturedCmd struct {
	dir  string
	env  []string
	name string
	args []string
}

// fakeImageBuilder records/reports image state.
type fakeImageBuilder struct {
	exists   bool
	built    bool
	buildErr error
}

func (f *fakeImageBuilder) ImageExists(context.Context, string) (bool, error) { return f.exists, nil }
func (f *fakeImageBuilder) ImageBuild(_ context.Context, _, _, _ string, _ io.Writer) error {
	f.built = true
	return f.buildErr
}
func (f *fakeImageBuilder) ImageDigest(context.Context, string) (string, error) {
	return "sha256:deadbeef", nil
}

func newTestBuilder(t *testing.T, docker ImageBuilder, cred CredentialFetcher) (*Builder, *[]capturedCmd) {
	t.Helper()
	var cmds []capturedCmd
	b := &Builder{
		runner: func(_ context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
			cmds = append(cmds, capturedCmd{dir: dir, env: env, name: name, args: args})
			return nil, nil
		},
		cred:     cred,
		docker:   docker,
		workRoot: t.TempDir(),
		log:      slog.Default(),
	}
	return b, &cmds
}

func op(t *testing.T, kind string, spec any) dsd.Op {
	t.Helper()
	b, _ := json.Marshal(spec)
	return dsd.Op{Kind: kind, Spec: b}
}

const testToken = "ghs_SUPERSECRET_TOKEN_1234567890"

// TestGitCloneTokenNeverInArgv is the load-bearing security assertion: the clone
// credential is passed to git ONLY via the environment, never in argv (where a
// process listing would leak it) and never in a file.
func TestGitCloneTokenNeverInArgv(t *testing.T) {
	cred := func(context.Context, string) (string, error) { return testToken, nil }
	b, cmds := newTestBuilder(t, &fakeImageBuilder{}, cred)

	err := b.opGitClone(context.Background(), op(t, KindGitClone, GitCloneSpec{
		ResourceID: "res_a", Provider: "github", RepoFullName: "acme/app",
		SHA: "abc1234def5678", CredentialRef: "dep_1",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(*cmds) != 4 {
		t.Fatalf("expected 4 git steps, got %d", len(*cmds))
	}
	tokenInEnv := false
	for _, c := range *cmds {
		for _, a := range c.args {
			if strings.Contains(a, testToken) {
				t.Fatalf("TOKEN LEAKED into argv: %v", c.args)
			}
		}
		for _, e := range c.env {
			if e == cloneCredEnv+"="+testToken {
				tokenInEnv = true
			} else if strings.Contains(e, testToken) {
				t.Fatalf("token in an unexpected env var: %q", e)
			}
		}
	}
	if !tokenInEnv {
		t.Error("token must be passed via the SIGMAHUB_GIT_TOKEN environment variable")
	}
	// The fetch step must install the env-reading credential helper (token-free).
	foundHelper := false
	for _, c := range *cmds {
		for _, a := range c.args {
			if strings.HasPrefix(a, "credential.helper=!f()") {
				foundHelper = true
			}
		}
	}
	if !foundHelper {
		t.Error("credentialed clone must install the env-reading credential helper")
	}
}

func TestGitCloneNoCredentialForPublicRepo(t *testing.T) {
	// No credential ref → no token env, no helper arg.
	b, cmds := newTestBuilder(t, &fakeImageBuilder{}, nil)
	if err := b.opGitClone(context.Background(), op(t, KindGitClone, GitCloneSpec{
		ResourceID: "res_a", RepoFullName: "acme/app", SHA: "abcdef1",
	})); err != nil {
		t.Fatal(err)
	}
	for _, c := range *cmds {
		for _, e := range c.env {
			if strings.HasPrefix(e, cloneCredEnv+"=") {
				t.Error("public clone must not set a token env var")
			}
		}
		for _, a := range c.args {
			if strings.HasPrefix(a, "credential.helper=!f()") {
				t.Error("public clone must not install a credential helper")
			}
		}
	}
}

func TestGitCloneRejectsBadInput(t *testing.T) {
	b, _ := newTestBuilder(t, &fakeImageBuilder{}, nil)
	for _, tc := range []GitCloneSpec{
		{ResourceID: "r", RepoFullName: "no-slash", SHA: "abcdef1"},
		{ResourceID: "r", RepoFullName: "a/b; rm -rf /", SHA: "abcdef1"},
		{ResourceID: "r", RepoFullName: "acme/app", SHA: "not-a-sha!"},
		{ResourceID: "r", RepoFullName: "acme/app", SHA: "abcdef1", Provider: "evilhost"},
	} {
		if err := b.opGitClone(context.Background(), op(t, KindGitClone, tc)); err == nil {
			t.Errorf("expected rejection for %+v", tc)
		}
	}
}

func TestCloneURL(t *testing.T) {
	u, err := cloneURL("github", "acme/app")
	if err != nil || u != "https://github.com/acme/app.git" {
		t.Fatalf("cloneURL = %q, %v", u, err)
	}
	if strings.Contains(u, testToken) {
		t.Error("clone URL must never embed a token")
	}
	if _, err := cloneURL("bogus", "a/b"); err == nil {
		t.Error("unknown provider must be rejected")
	}
}

// TestBuildImageDedupSkips proves a retry whose image already exists skips the
// build (idempotency / rebuild-free).
func TestBuildImageDedupSkips(t *testing.T) {
	fib := &fakeImageBuilder{exists: true}
	b, _ := newTestBuilder(t, fib, nil)
	err := b.opBuildImage(context.Background(), op(t, KindImageBuild, BuildImageSpec{
		ResourceID: "res_a", ImageTag: "sigmahub/res_a:abc", SHA: "abc",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if fib.built {
		t.Error("an existing image must not be rebuilt (dedup)")
	}
}

func TestBuildImageRequiresTag(t *testing.T) {
	b, _ := newTestBuilder(t, &fakeImageBuilder{}, nil)
	if err := b.opBuildImage(context.Background(), op(t, KindImageBuild, BuildImageSpec{ResourceID: "r"})); err == nil {
		t.Error("empty image tag must be rejected")
	}
}

// TestBuildForceRebuildsDespiteExistingImage proves a forced build (manual
// redeploy) bypasses the ImageExists dedup and rebuilds, while a normal build of
// an existing image is skipped.
func TestBuildForceRebuildsDespiteExistingImage(t *testing.T) {
	// Without Force: an existing image short-circuits (no rebuild).
	fb := &fakeImageBuilder{exists: true}
	b, _ := newTestBuilder(t, fb, nil)
	if err := b.opBuildImage(context.Background(), op(t, KindImageBuild, BuildImageSpec{
		ResourceID: "res_a", ImageTag: "sigmahub/res_a:sha", DedupKey: "k",
	})); err != nil {
		t.Fatal(err)
	}
	if fb.built {
		t.Fatal("an existing image without Force must be reused, not rebuilt")
	}

	// With Force: the same existing image is rebuilt.
	fb2 := &fakeImageBuilder{exists: true}
	b2, _ := newTestBuilder(t, fb2, nil)
	// Seed a Dockerfile in the context so the build path proceeds.
	dir := b2.ContextDir("res_a")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := b2.opBuildImage(context.Background(), op(t, KindImageBuild, BuildImageSpec{
		ResourceID: "res_a", ImageTag: "sigmahub/res_a:sha", DedupKey: "k", Force: true,
	})); err != nil {
		t.Fatal(err)
	}
	if !fb2.built {
		t.Fatal("Force must rebuild even when the image already exists")
	}
}
