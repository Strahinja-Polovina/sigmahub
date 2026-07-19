package build

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Strahinja-Polovina/sigmahub/agent/internal/apply"
	"github.com/Strahinja-Polovina/sigmahub/agent/internal/dsd"
)

// Runner executes a binary with a working directory and EXTRA environment (the
// clone credential rides here, never in argv). Swapped in tests.
type Runner func(ctx context.Context, dir string, extraEnv []string, name string, args ...string) ([]byte, error)

// CredentialFetcher resolves a short-lived clone credential from the control
// plane into memory. Returns "" (no error) when the ref carries no credential.
type CredentialFetcher func(ctx context.Context, credentialRef string) (token string, err error)

// ImageBuilder is the image side of the Docker client the build path needs.
type ImageBuilder interface {
	ImageExists(ctx context.Context, ref string) (bool, error)
	ImageBuild(ctx context.Context, contextDir, dockerfile, tag string, logs io.Writer) error
	ImageDigest(ctx context.Context, ref string) (string, error)
}

// LogSink streams a build/orchestration log line (to the control plane). Nil-safe
// via Builder.stream.
type LogSink func(ctx context.Context, deploymentID, stream, line string)

// Builder registers the git.clone and image.build ops.
type Builder struct {
	runner   Runner
	cred     CredentialFetcher
	docker   ImageBuilder
	workRoot string
	log      *slog.Logger
	sink     LogSink
}

// NewBuilder wires the default os/exec runner. workRoot is the base directory for
// per-resource build contexts.
func NewBuilder(docker ImageBuilder, cred CredentialFetcher, workRoot string, log *slog.Logger, sink LogSink) *Builder {
	return &Builder{
		runner: func(ctx context.Context, dir string, extraEnv []string, name string, args ...string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, name, args...)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), extraEnv...)
			return cmd.CombinedOutput()
		},
		cred: cred, docker: docker, workRoot: workRoot, log: log, sink: sink,
	}
}

// Register hooks the typed ops into the apply registry (the only place a new
// capability can be added — the no-generic-run-shell invariant).
func (b *Builder) Register(r *apply.Registry) {
	r.Register(KindGitClone, b.opGitClone)
	r.Register(KindImageBuild, b.opBuildImage)
}

var (
	repoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	shaRe  = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)
)

// ContextDir is the per-resource build context path.
func (b *Builder) ContextDir(resourceID string) string {
	return filepath.Join(b.workRoot, resourceID)
}

// providerHost maps a provider to its clone host. Only known providers are
// allowed, so a spec can't point the clone at an arbitrary host.
func providerHost(provider string) (string, bool) {
	switch provider {
	case "", "github":
		return "github.com", true
	case "gitlab":
		return "gitlab.com", true
	}
	return "", false
}

// cloneURL builds the HTTPS clone URL. The token is NEVER embedded here — it is
// supplied to git via the environment + a credential helper.
func cloneURL(provider, repoFullName string) (string, error) {
	host, ok := providerHost(provider)
	if !ok {
		return "", fmt.Errorf("unsupported provider %q", provider)
	}
	return "https://" + host + "/" + repoFullName + ".git", nil
}

// cloneCredEnv is the env var the credential helper reads the token from.
const cloneCredEnv = "SIGMAHUB_GIT_TOKEN"

// credHelperArg is git's credential.helper value: an inline shell function that
// echoes the token FROM THE ENVIRONMENT. The token itself never appears in argv
// or on disk — only this fixed, token-free helper string does.
const credHelperArg = `credential.helper=!f() { echo username=x-access-token; echo "password=$` + cloneCredEnv + `"; }; f`

// cloneSteps is the fixed git command sequence to materialise a repo at a SHA:
// init an empty repo, add the remote, fetch exactly the wanted commit (shallow),
// and detach onto it. Deterministic and free of shell metacharacters — the SHA
// and repo are validated by the caller before this runs.
func cloneSteps(dir, url, sha string, withCred bool) [][]string {
	fetch := []string{"-C", dir}
	if withCred {
		// Disable inherited helpers, then install the env-reading one.
		fetch = append(fetch, "-c", "credential.helper=", "-c", credHelperArg)
	}
	fetch = append(fetch, "fetch", "--depth", "1", "origin", sha)
	return [][]string{
		{"init", "-q", dir},
		{"-C", dir, "remote", "add", "origin", url},
		fetch,
		{"-C", dir, "checkout", "-q", "--detach", "FETCH_HEAD"},
	}
}

func (b *Builder) opGitClone(ctx context.Context, op dsd.Op) error {
	var spec GitCloneSpec
	if err := json.Unmarshal(op.Spec, &spec); err != nil {
		return fmt.Errorf("decode git.clone spec: %w", err)
	}
	if !repoRe.MatchString(spec.RepoFullName) {
		return fmt.Errorf("invalid repo %q", spec.RepoFullName)
	}
	if !shaRe.MatchString(spec.SHA) {
		return fmt.Errorf("invalid sha %q", spec.SHA)
	}
	url, err := cloneURL(spec.Provider, spec.RepoFullName)
	if err != nil {
		return err
	}

	dir := b.ContextDir(spec.ResourceID)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("clean context dir: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create context dir: %w", err)
	}

	// Resolve the clone credential into memory (never persisted). Passed to git
	// only via the environment.
	var extraEnv []string
	withCred := false
	if spec.CredentialRef != "" && b.cred != nil {
		tok, err := b.cred(ctx, spec.CredentialRef)
		if err != nil {
			return fmt.Errorf("resolve clone credential: %w", err)
		}
		if tok != "" {
			extraEnv = append(extraEnv, cloneCredEnv+"="+tok)
			withCred = true
		}
	}

	for _, args := range cloneSteps(dir, url, spec.SHA, withCred) {
		if out, err := b.runner(ctx, "", extraEnv, "git", args...); err != nil {
			// Never echo the environment; out is git's own stderr (no token in it).
			return fmt.Errorf("git %s failed: %w: %s", args[0], err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

func (b *Builder) opBuildImage(ctx context.Context, op dsd.Op) error {
	var spec BuildImageSpec
	if err := json.Unmarshal(op.Spec, &spec); err != nil {
		return fmt.Errorf("decode image.build spec: %w", err)
	}
	if spec.ImageTag == "" {
		return fmt.Errorf("image.build: empty image tag")
	}

	// Dedup: a retry of the same inputs finds the image already built and skips —
	// the idempotency invariant (and what keeps a rollback rebuild-free). A forced
	// build (manual redeploy) bypasses the short-circuit to rebuild the same commit.
	if !spec.Force {
		if exists, err := b.docker.ImageExists(ctx, spec.ImageTag); err != nil {
			return err
		} else if exists {
			b.stream(ctx, spec.DeploymentID, "build", "image "+spec.ImageTag+" already built — reusing")
			return nil
		}
	}

	dockerfile := spec.Dockerfile
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	dir := b.ContextDir(spec.ResourceID)
	// A Compose service builds from a subdir of the clone. Confine it to the clone
	// so a crafted spec can't escape the build root (path traversal).
	if spec.ContextSubdir != "" {
		sub := filepath.Clean("/" + spec.ContextSubdir)[1:] // strip leading .. / abs
		dir = filepath.Join(dir, sub)
		root := b.ContextDir(spec.ResourceID)
		if rel, err := filepath.Rel(root, dir); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("build context %q escapes the clone root", spec.ContextSubdir)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, dockerfile)); err != nil {
		return fmt.Errorf("build context missing %s (clone did not run?): %w", dockerfile, err)
	}

	logs := &lineWriter{ctx: ctx, sink: b.sink, deploymentID: spec.DeploymentID}
	if err := b.docker.ImageBuild(ctx, dir, dockerfile, spec.ImageTag, logs); err != nil {
		return fmt.Errorf("build image: %w", err)
	}
	return nil
}

func (b *Builder) stream(ctx context.Context, deploymentID, streamName, line string) {
	if b.sink != nil {
		b.sink(ctx, deploymentID, streamName, line)
	}
}

// lineWriter turns a build-output stream into per-line log-sink calls.
type lineWriter struct {
	ctx          context.Context
	sink         LogSink
	deploymentID string
	buf          []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	if w.sink == nil {
		return len(p), nil
	}
	w.buf = append(w.buf, p...)
	for {
		i := indexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(w.buf[:i]), "\r")
		w.buf = w.buf[i+1:]
		if line != "" {
			w.sink(w.ctx, w.deploymentID, "build", line)
		}
	}
	// Backstop: a very long line with no newline (a hostile/broken Dockerfile can
	// emit gigabytes) is flushed in chunks rather than growing the buffer without
	// bound and OOM-ing the agent — mirrors logLineSplitter (SIGMA-98).
	if len(w.buf) > 64*1024 {
		if line := strings.TrimRight(string(w.buf), "\r"); line != "" {
			w.sink(w.ctx, w.deploymentID, "build", line)
		}
		w.buf = nil
	}
	return len(p), nil
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}
