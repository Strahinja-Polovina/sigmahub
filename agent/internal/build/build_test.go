package build

import (
	"context"
	"encoding/json"
	"errors"
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
	pushed   bool
	pushErr  error
	pushAuth RegistryAuth
}

func (f *fakeImageBuilder) ImageExists(context.Context, string) (bool, error) { return f.exists, nil }
func (f *fakeImageBuilder) ImageBuild(_ context.Context, _, _, _ string, _ io.Writer) error {
	f.built = true
	return f.buildErr
}
func (f *fakeImageBuilder) ImageDigest(context.Context, string) (string, error) {
	return "sha256:deadbeef", nil
}
func (f *fakeImageBuilder) ImagePush(_ context.Context, _ string, auth RegistryAuth, _ io.Writer) error {
	f.pushAuth = auth
	f.pushed = true
	return f.pushErr
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
		// The build host has nixpacks; a test that needs the opposite overrides
		// this. Resolving against the REAL PATH would make these tests pass or
		// fail depending on what happens to be installed on the machine.
		lookPath: func(name string) (string, error) { return "/usr/local/bin/" + name, nil },
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

// A dedicated build server must PUSH what it builds: the deploy target cannot
// read this machine's Docker daemon, so an unpushed image leaves the rollout
// waiting for something that never arrives.
func TestBuildPushesForAnotherHost(t *testing.T) {
	fb := &fakeImageBuilder{}
	b, _ := newTestBuilder(t, fb, nil)
	dir := b.ContextDir("res_1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	spec, _ := json.Marshal(BuildImageSpec{
		ResourceID: "res_1", SHA: "abc", ImageTag: "reg.example/app:abc", PushImage: true,
	})
	if err := b.opBuildImage(context.Background(), dsd.Op{Kind: KindImageBuild, Spec: spec}); err != nil {
		t.Fatal(err)
	}
	if !fb.built || !fb.pushed {
		t.Fatalf("built=%v pushed=%v, want both", fb.built, fb.pushed)
	}
}

// A push to a real registry has to authenticate. This used to send
// base64("{}") unconditionally — an anonymous push, which every hosted registry
// answers with a 401, so the dedicated build server could never actually ship
// an image anywhere.
func TestPushCarriesTheRegistryCredential(t *testing.T) {
	fb := &fakeImageBuilder{}
	b, _ := newTestBuilder(t, fb, nil)
	b.registry = func(context.Context) (RegistryAuth, error) {
		return RegistryAuth{Host: "ghcr.io", Username: "bot", Password: "s3cret"}, nil
	}
	dir := b.ContextDir("res_auth")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec, _ := json.Marshal(BuildImageSpec{
		ResourceID: "res_auth", SHA: "abc", ImageTag: "ghcr.io/acme/app:abc",
		PushImage: true, RegistryHost: "ghcr.io",
	})
	if err := b.opBuildImage(context.Background(), dsd.Op{Kind: KindImageBuild, Spec: spec}); err != nil {
		t.Fatal(err)
	}
	if fb.pushAuth.Username != "bot" || fb.pushAuth.Password != "s3cret" || fb.pushAuth.Host != "ghcr.io" {
		t.Fatalf("push auth = %+v, want the org's registry credential", fb.pushAuth)
	}
}

// No way to resolve the credential must fail the op HERE, naming the cause.
// Pushing anonymously instead surfaces much later as an unexplained pull
// failure on whichever machine was supposed to run the image.
func TestPushWithoutACredentialSourceFails(t *testing.T) {
	fb := &fakeImageBuilder{}
	b, _ := newTestBuilder(t, fb, nil) // no registry fetcher wired
	dir := b.ContextDir("res_noauth")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec, _ := json.Marshal(BuildImageSpec{
		ResourceID: "res_noauth", SHA: "abc", ImageTag: "ghcr.io/acme/app:abc",
		PushImage: true, RegistryHost: "ghcr.io",
	})
	err := b.opBuildImage(context.Background(), dsd.Op{Kind: KindImageBuild, Spec: spec})
	if err == nil {
		t.Fatal("a push that needs a credential and has none must fail loudly")
	}
	if fb.pushed {
		t.Fatal("nothing must be pushed anonymously to a registry that requires auth")
	}
	if !strings.Contains(err.Error(), "ghcr.io") {
		t.Fatalf("error must name the registry: %v", err)
	}
}

// A push failure must fail the op. Reporting a successful build whose image
// nobody can pull would wedge the deploy target with no explanation.
func TestBuildFailsWhenPushFails(t *testing.T) {
	fb := &fakeImageBuilder{pushErr: errors.New("registry unreachable")}
	b, _ := newTestBuilder(t, fb, nil)
	dir := b.ContextDir("res_2")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec, _ := json.Marshal(BuildImageSpec{
		ResourceID: "res_2", SHA: "abc", ImageTag: "reg.example/app:abc", PushImage: true,
	})
	err := b.opBuildImage(context.Background(), dsd.Op{Kind: KindImageBuild, Spec: spec})
	if err == nil {
		t.Fatal("a failed push must fail the build op")
	}
	if !strings.Contains(err.Error(), "registry unreachable") {
		t.Fatalf("error must carry the cause: %v", err)
	}
}

// Building for THIS host never pushes — there is no registry round trip to pay
// for when the deploy target is the machine that just built it.
func TestBuildDoesNotPushForLocalDeploy(t *testing.T) {
	fb := &fakeImageBuilder{}
	b, _ := newTestBuilder(t, fb, nil)
	dir := b.ContextDir("res_3")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec, _ := json.Marshal(BuildImageSpec{ResourceID: "res_3", SHA: "abc", ImageTag: "app:abc"})
	if err := b.opBuildImage(context.Background(), dsd.Op{Kind: KindImageBuild, Spec: spec}); err != nil {
		t.Fatal(err)
	}
	if fb.pushed {
		t.Fatal("a local build must not push")
	}
}

// ── The nixpacks builder (the auto-build fallback) ──────────────────────────

// A repository that ships no Dockerfile is built by nixpacks from its language
// manifest. The old path stat'd for a Dockerfile before doing anything, so this
// spec would have failed with "build context missing Dockerfile (clone did not
// run?)" — an error about the wrong step entirely.
func TestBuildImageNixpacksBuildsWithoutADockerfile(t *testing.T) {
	docker := &fakeImageBuilder{}
	b, cmds := newTestBuilder(t, docker, nil)
	dir := b.ContextDir("res_nix")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A Go service: a go.mod and no Dockerfile anywhere.
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module acme/x\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := b.opBuildImage(context.Background(), op(t, KindImageBuild, BuildImageSpec{
		ResourceID: "res_nix", SHA: "abc1234", ImageTag: "sigmahub/res_nix:abc1234",
		Builder: BuilderNixpacks,
	})); err != nil {
		t.Fatalf("nixpacks build: %v", err)
	}
	if docker.built {
		t.Error("a nixpacks build must not also run a docker build of a Dockerfile that does not exist")
	}
	var nix *capturedCmd
	for i := range *cmds {
		if (*cmds)[i].name == "nixpacks" {
			nix = &(*cmds)[i]
		}
	}
	if nix == nil {
		t.Fatalf("nixpacks was never invoked; commands = %+v", *cmds)
	}
	if nix.dir != dir {
		t.Errorf("nixpacks ran in %q, want the clone at %q", nix.dir, dir)
	}
	if strings.Join(nix.args, " ") != "build . --name sigmahub/res_nix:abc1234" {
		t.Errorf("nixpacks args = %v, want the image tag it must produce", nix.args)
	}
}

// The context subdirectory is honoured by the nixpacks path too: a monorepo's
// app is built where it lives, not at the clone root.
func TestBuildImageNixpacksHonoursContextSubdir(t *testing.T) {
	b, cmds := newTestBuilder(t, &fakeImageBuilder{}, nil)
	sub := filepath.Join(b.ContextDir("res_mono"), "apps", "api")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := b.opBuildImage(context.Background(), op(t, KindImageBuild, BuildImageSpec{
		ResourceID: "res_mono", ImageTag: "sigmahub/res_mono:x",
		Builder: BuilderNixpacks, ContextSubdir: "apps/api",
	})); err != nil {
		t.Fatal(err)
	}
	for _, c := range *cmds {
		if c.name == "nixpacks" && c.dir != sub {
			t.Errorf("nixpacks ran in %q, want %q", c.dir, sub)
		}
	}
}

// A context subdirectory cannot escape the clone, whichever builder runs — the
// nixpacks path must not become a way around the confinement the docker path
// has, because it hands the directory to a subprocess rather than to the
// daemon.
func TestBuildImageNixpacksContextStaysInsideTheClone(t *testing.T) {
	b, cmds := newTestBuilder(t, &fakeImageBuilder{}, nil)
	root := b.ContextDir("res_esc")
	if err := b.opBuildImage(context.Background(), op(t, KindImageBuild, BuildImageSpec{
		ResourceID: "res_esc", ImageTag: "t", Builder: BuilderNixpacks, ContextSubdir: "../../etc",
	})); err != nil {
		t.Fatal(err)
	}
	for _, c := range *cmds {
		if c.name != "nixpacks" {
			continue
		}
		if !strings.HasPrefix(c.dir, root+string(filepath.Separator)) && c.dir != root {
			t.Errorf("nixpacks ran in %q, outside the clone at %q", c.dir, root)
		}
	}
}

// An unknown builder fails the op instead of quietly falling back to looking
// for a Dockerfile: the resulting "build context missing Dockerfile" would blame
// the clone for a control-plane typo.
func TestBuildImageUnknownBuilderRefused(t *testing.T) {
	docker := &fakeImageBuilder{}
	b, _ := newTestBuilder(t, docker, nil)
	dir := b.ContextDir("res_bad")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := b.opBuildImage(context.Background(), op(t, KindImageBuild, BuildImageSpec{
		ResourceID: "res_bad", ImageTag: "t", Builder: "buildpacks",
	}))
	if err == nil || !strings.Contains(err.Error(), "unknown builder") {
		t.Fatalf("unknown builder must fail loudly, got %v", err)
	}
	if docker.built {
		t.Error("an unknown builder must not silently run a docker build")
	}
}

// The default (empty) builder is unchanged: a docker build of the Dockerfile.
func TestBuildImageDefaultBuilderStillDockerfile(t *testing.T) {
	docker := &fakeImageBuilder{}
	b, cmds := newTestBuilder(t, docker, nil)
	dir := b.ContextDir("res_df")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := b.opBuildImage(context.Background(), op(t, KindImageBuild, BuildImageSpec{
		ResourceID: "res_df", ImageTag: "t",
	})); err != nil {
		t.Fatal(err)
	}
	if !docker.built {
		t.Error("the default path must still docker build")
	}
	for _, c := range *cmds {
		if c.name == "nixpacks" {
			t.Error("the default path must not shell out to nixpacks")
		}
	}
}

// A host without nixpacks must say so, not fail with "executable file not
// found in $PATH".
//
// The auto-build method exists to answer "this repository does not say how to
// build itself" — the worst dead end in the wizard. Answering it with a
// low-level exec error, on a fleet installed before nixpacks was part of the
// install script, moves the dead end rather than removing it: the operator sees
// a broken build for a repository that is fine.
func TestBuildImageNixpacksSaysWhatIsMissing(t *testing.T) {
	b, cmds := newTestBuilder(t, &fakeImageBuilder{}, nil)
	b.lookPath = func(string) (string, error) { return "", errors.New("not found") }

	dir := b.ContextDir("res_nonix")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module acme/x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := b.opBuildImage(context.Background(), op(t, KindImageBuild, BuildImageSpec{
		ResourceID: "res_nonix", SHA: "abc1234", ImageTag: "sigmahub/res_nonix:abc1234",
		Builder: BuilderNixpacks,
	}))
	if err == nil {
		t.Fatal("a host with no nixpacks must refuse the build")
	}
	// The message has to name the missing thing AND both ways out, because the
	// operator reading it has no reason to know the auto-build method is a
	// separate binary.
	for _, want := range []string{"nixpacks", "installer", "Dockerfile build method"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	for _, c := range *cmds {
		if c.name == "nixpacks" {
			t.Fatal("nixpacks was executed despite not being on the host")
		}
	}
}
