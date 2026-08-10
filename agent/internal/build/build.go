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
	"sync"
	"time"

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
	// ImagePush publishes a built image so another host can pull it. Only used
	// by a dedicated build server, which builds for machines that cannot read
	// its local Docker daemon. auth carries the registry credential; a zero
	// value pushes anonymously.
	ImagePush(ctx context.Context, ref string, auth RegistryAuth, logs io.Writer) error
}

// RegistryAuth is the credential a push authenticates with. A zero value means
// an anonymous push — which every hosted registry refuses, and which is exactly
// what this used to do unconditionally.
type RegistryAuth struct {
	Host     string `json:"serveraddress,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

// RegistryFetcher resolves the org's registry credential from the control
// plane. Nil on a host that never pushes.
type RegistryFetcher func(ctx context.Context) (RegistryAuth, error)

// LogSink ships a BATCH of build/orchestration log lines (to the control plane).
// Nil-safe via Builder.stream.
//
// It takes a slice, not a line, because the sink is a blocking HTTPS POST and a
// build emits thousands of lines: one call per line is one round trip per line
// on the goroutine draining Docker's output pipe (SIGMA-252). lineWriter does
// the buffering; the sink just ships what it is handed.
type LogSink func(ctx context.Context, deploymentID, stream string, lines []string)

// Builder registers the git.clone and image.build ops.
type Builder struct {
	runner   Runner
	cred     CredentialFetcher
	registry RegistryFetcher
	docker   ImageBuilder
	workRoot string
	log      *slog.Logger
	sink     LogSink
	// lookPath resolves an external builder on PATH. A field so a test can say
	// "this host has no nixpacks" without depending on the build machine.
	lookPath func(string) (string, error)
}

// NewBuilder wires the default os/exec runner. workRoot is the base directory for
// per-resource build contexts. registry resolves the push credential and may be
// nil on a host that never builds for another machine.
func NewBuilder(docker ImageBuilder, cred CredentialFetcher, registry RegistryFetcher, workRoot string, log *slog.Logger, sink LogSink) *Builder {
	return &Builder{
		lookPath: exec.LookPath,
		runner: func(ctx context.Context, dir string, extraEnv []string, name string, args ...string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, name, args...)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), extraEnv...)
			return cmd.CombinedOutput()
		},
		cred: cred, registry: registry, docker: docker, workRoot: workRoot, log: log, sink: sink,
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

// ContextDir is the per-resource build context path. Callers that act on the
// path (opGitClone, opBuildImage) must resolve it through checkedContextDir
// instead — see the note there.
func (b *Builder) ContextDir(resourceID string) string {
	return filepath.Join(b.workRoot, resourceID)
}

// checkedContextDir resolves the per-resource build context and refuses any
// resource id that is not a single path segment inside workRoot (SIGMA-341).
//
// The resource id is the OUTER path segment of the context directory, and
// opGitClone os.RemoveAll's that directory as root before it does anything
// else. filepath.Join CLEANS its result, so an id containing ".." silently
// escapes workRoot: a resourceId of "../../../../etc" resolves to /etc and the
// clone deletes it. Every neighbouring input on this path is already checked —
// repoRe, shaRe, the provider host allow-list, and the ContextSubdir escape
// check in opBuildImage below — but the id, the one segment with the most
// destructive reach, was not.
//
// The threat model is the same one the container policy is written against: a
// compromised or buggy control plane whose DSD still signs correctly. Today ids
// are CP-generated ("res_<hex>") so this is a missing guard rather than a live
// exploit; one store or API bug that lets a caller influence a resource id would
// turn it into fleet-wide destruction. The check is deliberately a confinement
// check rather than a match against the CP's current id spelling — agent and
// control plane are separate modules, and a future id scheme must not brick
// every agent's builds, but no id may ever step outside the build root.
func (b *Builder) checkedContextDir(resourceID string) (string, error) {
	if resourceID == "" {
		return "", fmt.Errorf("invalid resourceId: empty")
	}
	if strings.ContainsAny(resourceID, `/\`) || resourceID != filepath.Clean(resourceID) || resourceID == "." || resourceID == ".." {
		return "", fmt.Errorf("invalid resourceId %q: must be a single path segment", resourceID)
	}
	dir := b.ContextDir(resourceID)
	rel, err := filepath.Rel(b.workRoot, dir)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("invalid resourceId %q: escapes the build root", resourceID)
	}
	return dir, nil
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
	// Validate the id BEFORE any filesystem call: the very next thing this op
	// does is RemoveAll the path built from it, as root (SIGMA-341).
	dir, err := b.checkedContextDir(spec.ResourceID)
	if err != nil {
		return err
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
	// The id is the outer segment of the same path the subdir check below
	// confines against — an unchecked one makes that check meaningless, because
	// the "root" it measures against has itself already escaped (SIGMA-341).
	root, err := b.checkedContextDir(spec.ResourceID)
	if err != nil {
		return err
	}
	dir := root
	// A Compose service builds from a subdir of the clone. Confine it to the clone
	// so a crafted spec can't escape the build root (path traversal).
	if spec.ContextSubdir != "" {
		sub := filepath.Clean("/" + spec.ContextSubdir)[1:] // strip leading .. / abs
		dir = filepath.Join(dir, sub)
		if rel, err := filepath.Rel(root, dir); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("build context %q escapes the clone root", spec.ContextSubdir)
		}
	}
	logs := newLineWriter(ctx, b.sink, spec.DeploymentID)
	// Ships the tail on every exit path — including the failure paths, whose last
	// buffered lines are exactly the ones explaining the failure.
	defer func() { _ = logs.Close() }()
	switch spec.Builder {
	case "", BuilderDockerfile:
		if _, err := os.Stat(filepath.Join(dir, dockerfile)); err != nil {
			return fmt.Errorf("build context missing %s (clone did not run?): %w", dockerfile, err)
		}
		if err := b.docker.ImageBuild(ctx, dir, dockerfile, spec.ImageTag, logs); err != nil {
			return fmt.Errorf("build image: %w", err)
		}
	case BuilderNixpacks:
		// nixpacks reads the source tree, derives a build plan from its language
		// manifest and drives docker build itself — which is exactly why it is
		// the fallback we chose: it needs no new op kind and no privileged
		// surface beyond the docker access this path already has.
		// Say what is missing rather than letting exec fail with "executable
		// file not found": an operator reading a deploy log should not have to
		// know that the auto-build method is a separate binary the installer
		// puts on the host.
		if _, err := b.lookPath("nixpacks"); err != nil {
			return fmt.Errorf("this host has no nixpacks, so a repository with no Dockerfile cannot be auto-built here — " +
				"re-run the agent installer to add it, or switch the app to the Dockerfile build method")
		}
		b.stream(ctx, spec.DeploymentID, "build", "no Dockerfile — building "+spec.ImageTag+" with nixpacks")
		out, err := b.runner(ctx, dir, nil, "nixpacks", "build", ".", "--name", spec.ImageTag)
		// nixpacks streams the underlying docker build on its own stdout, so its
		// output is the build log whether it succeeded or not.
		_, _ = logs.Write(out)
		if err != nil {
			return fmt.Errorf("nixpacks build: %w: %s", err, strings.TrimSpace(lastLines(string(out), 20)))
		}
	default:
		return fmt.Errorf("unknown builder %q — this agent knows %q and %q", spec.Builder, BuilderDockerfile, BuilderNixpacks)
	}
	// Dedicated build server: the deploy target pulls this image, and it cannot
	// read this machine's Docker daemon. A push failure has to fail the op — a
	// "successful" build whose image nobody can pull would leave the rollout
	// waiting on an image that never arrives.
	if spec.PushImage {
		// Authenticate. An anonymous push to a hosted registry is a 401, so a
		// missing credential has to fail the op here with a message that names
		// the cause rather than surfacing later as an unexplained pull failure
		// on whichever machine was supposed to run the image.
		var auth RegistryAuth
		if spec.RegistryHost != "" {
			if b.registry == nil {
				return fmt.Errorf("push to %s needs a registry credential and this agent has no way to fetch one", spec.RegistryHost)
			}
			got, err := b.registry(ctx)
			if err != nil {
				return fmt.Errorf("resolve registry credential for %s: %w", spec.RegistryHost, err)
			}
			auth = got
			if auth.Host == "" {
				auth.Host = spec.RegistryHost
			}
		}
		// Through the log writer, not b.stream: the build output ahead of this is
		// still buffered, and a direct sink call would land in the deploy view
		// BEFORE the build lines it is supposed to follow.
		_, _ = logs.Write([]byte("pushing " + spec.ImageTag + " for the deploy target\n"))
		if err := b.docker.ImagePush(ctx, spec.ImageTag, auth, logs); err != nil {
			return fmt.Errorf("push image %s: %w", spec.ImageTag, err)
		}
	}
	return nil
}

func (b *Builder) stream(ctx context.Context, deploymentID, streamName, line string) {
	if b.sink != nil {
		b.sink(ctx, deploymentID, streamName, []string{line})
	}
}

const (
	// logBatchSize is how many buffered lines trigger an immediate ship. A
	// typical Node or Python Dockerfile emits a few thousand lines, so this
	// turns a build into a couple of dozen requests instead of a couple of
	// thousand.
	logBatchSize = 200
	// logFlushInterval bounds how long a partial batch waits. A build that
	// prints one line every few seconds must still show up in the deploy view
	// promptly — batching may not turn the live log into a slideshow.
	logFlushInterval = 500 * time.Millisecond
	// logBufferCap bounds the unshipped backlog. Docker's output pipe
	// back-pressures whenever this writer blocks, so a control plane that
	// cannot keep up must cost dropped log lines, never a stalled build.
	logBufferCap = 20000
	// logShipMax is the largest batch handed to the sink in one call. It mirrors
	// the control plane's per-request cap on /v1/agent/build-logs, which
	// TRUNCATES anything longer — a backlog flushed in one go (or the final
	// flush of a fast build) would otherwise lose everything past the cap
	// silently, which is worse than the per-line shipping this replaced.
	logShipMax = 500
)

// lineWriter turns a build-output stream into BATCHED log-sink calls.
//
// It used to call the sink once per line, and the sink is one blocking HTTPS
// POST to the control plane plus one INSERT there (SIGMA-252). On the goroutine
// draining Docker's output pipe that meant a full agent→CP round trip of added
// latency per line: a 2,000-line build 40ms from the CP spent ~160 seconds of
// pure network wait on top of a build Docker had finished, with Docker's pipe
// back-pressuring the whole time, while the CP absorbed 2,000 authenticated
// requests and 2,000 indexed inserts for one build.
//
// So Write now only parses lines and appends them to a bounded buffer — it
// never touches the network — and a background goroutine ships whole batches on
// size or on a timer. Overflow is dropped and COUNTED (and the count is
// reported into the log itself), the same bargain the telemetry shipper makes:
// losing log lines is recoverable, wedging the build is not.
//
// Callers MUST Close the writer: the trailing partial line and the last partial
// batch are shipped there.
type lineWriter struct {
	ctx          context.Context
	sink         LogSink
	deploymentID string

	mu      sync.Mutex
	buf     []byte   // carry-over for a line split across Writes
	pending []string // parsed, unshipped lines
	dropped int

	wake     chan struct{} // size-triggered flush signal (non-blocking, cap 1)
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

func newLineWriter(ctx context.Context, sink LogSink, deploymentID string) *lineWriter {
	w := &lineWriter{
		ctx: ctx, sink: sink, deploymentID: deploymentID,
		wake: make(chan struct{}, 1),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	if sink == nil {
		close(w.done) // nothing to ship; Close must not block on a goroutine that never ran
		return w
	}
	go w.run()
	return w
}

// run is the shipping loop: everything that touches the sink happens here, off
// the Write path.
func (w *lineWriter) run() {
	defer close(w.done)
	t := time.NewTicker(logFlushInterval)
	defer t.Stop()
	for {
		select {
		case <-w.stop:
			return
		case <-w.ctx.Done():
			return
		case <-t.C:
		case <-w.wake:
		}
		w.flush()
	}
}

func (w *lineWriter) Write(p []byte) (int, error) {
	if w.sink == nil {
		return len(p), nil
	}
	w.mu.Lock()
	w.buf = append(w.buf, p...)
	for {
		i := indexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(w.buf[:i]), "\r")
		w.buf = w.buf[i+1:]
		if line != "" {
			w.enqueueLocked(line)
		}
	}
	// Backstop: a very long line with no newline (a hostile/broken Dockerfile can
	// emit gigabytes) is flushed in chunks rather than growing the buffer without
	// bound and OOM-ing the agent — mirrors logLineSplitter (SIGMA-98).
	if len(w.buf) > 64*1024 {
		if line := strings.TrimRight(string(w.buf), "\r"); line != "" {
			w.enqueueLocked(line)
		}
		w.buf = nil
	}
	full := len(w.pending) >= logBatchSize
	w.mu.Unlock()
	if full {
		select {
		case w.wake <- struct{}{}:
		default: // a ship is already pending; it will pick these up
		}
	}
	return len(p), nil
}

// enqueueLocked buffers one line, dropping (and counting) on overflow. Caller
// holds w.mu.
func (w *lineWriter) enqueueLocked(line string) {
	if len(w.pending) >= logBufferCap {
		w.dropped++
		return
	}
	w.pending = append(w.pending, line)
}

// flush ships whatever is buffered as ONE sink call. Only ever called from run
// (which is single-threaded) or from Close after run has exited, so the sink is
// never entered concurrently and line order is preserved.
func (w *lineWriter) flush() {
	w.mu.Lock()
	lines, dropped := w.pending, w.dropped
	w.pending, w.dropped = nil, 0
	w.mu.Unlock()
	if dropped > 0 {
		// Say so in the log rather than silently showing a build with holes in it.
		lines = append(lines, fmt.Sprintf("… %d build log line(s) dropped — the control plane could not keep up", dropped))
	}
	for len(lines) > 0 {
		n := min(len(lines), logShipMax)
		w.sink(w.ctx, w.deploymentID, "build", lines[:n])
		lines = lines[n:]
	}
}

// Close stops the shipping loop and ships the tail. Safe to call more than once.
func (w *lineWriter) Close() error {
	w.stopOnce.Do(func() { close(w.stop) })
	<-w.done
	w.mu.Lock()
	// A build whose last line has no trailing newline still has to reach the log —
	// that line is very often the one naming the failure.
	if line := strings.TrimRight(string(w.buf), "\r"); line != "" {
		w.enqueueLocked(line)
	}
	w.buf = nil
	w.mu.Unlock()
	w.flush()
	return nil
}

// lastLines is the TAIL of a build's output, for the error message. A failed
// build's cause is at the end; leading the error with the first 20 lines of
// "installing nix packages" would bury it.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}
