package container

// The authenticated-push e2e.
//
// A dedicated build server and every cluster workload depend on one thing:
// that an image built here reaches a registry the other machines can pull from.
// That path shipped pushing `X-Registry-Auth: base64("{}")` — an anonymous push
// — which every hosted registry answers with a 401, so it had never actually
// delivered an image anywhere. Nothing caught it because the only test used a
// fake Docker client, and a fake accepts any header you hand it.
//
// So this drives a REAL `docker push` against a real HTTP registry: enough of
// the OCI distribution API to accept an upload, behind a Basic-auth challenge.
// The daemon shares this process's loopback, and Docker treats 127.0.0.1 as an
// insecure registry by default, so no TLS or daemon config is involved.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/agent/internal/build"
)

// fakeRegistry is an OCI distribution endpoint that accepts pushes and records
// what credentials it was given.
//
// It is a real content-addressable store, not a stub that says yes: blobs are
// kept by digest and a HEAD answers from that. Docker's two image stores drive
// the push differently — the containerd snapshotter uploads and moves on, the
// classic one verifies each blob is present before it assembles the manifest —
// and a registry that 404s a blob it just accepted fails the second one with
// "unknown blob". Behaving correctly is cheaper than guessing which store the
// host happens to run.
type fakeRegistry struct {
	user, pass string

	mu      sync.Mutex
	uploads map[string][]byte // upload id -> bytes received so far
	blobs   map[string][]byte // digest -> content
	pushed  int               // finalized blob uploads
	// manifests is every manifest body accepted, in order.
	manifests []string
	// seenAuth is every Authorization header value the registry received.
	seenAuth []string
	// anonymous counts requests that arrived with no credentials at all.
	anonymous int
}

func newFakeRegistry(user, pass string) *fakeRegistry {
	return &fakeRegistry{
		user: user, pass: pass,
		uploads: map[string][]byte{},
		blobs:   map[string][]byte{},
	}
}

// digestOf is the content address the distribution API keys blobs by.
func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// authorized enforces the Basic challenge. Docker only sends credentials after
// a 401 names a scheme it understands, so the challenge is what makes the
// credential observable at all.
func (r *fakeRegistry) authorized(w http.ResponseWriter, req *http.Request) bool {
	got := req.Header.Get("Authorization")
	r.mu.Lock()
	if got == "" {
		r.anonymous++
	} else {
		r.seenAuth = append(r.seenAuth, got)
	}
	r.mu.Unlock()

	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(r.user+":"+r.pass))
	if got == want {
		return true
	}
	w.Header().Set("WWW-Authenticate", `Basic realm="sigmahub-test"`)
	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	http.Error(w, `{"errors":[{"code":"UNAUTHORIZED","message":"authentication required"}]}`,
		http.StatusUnauthorized)
	return false
}

func (r *fakeRegistry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	if !r.authorized(w, req) {
		return
	}
	path := req.URL.Path

	switch {
	// Version probe.
	case path == "/v2" || path == "/v2/":
		w.WriteHeader(http.StatusOK)

	// Start an upload. A cross-repository mount is declined by simply opening a
	// normal upload session, which is what a registry that does not support it
	// does; the client then uploads the bytes.
	case strings.HasSuffix(path, "/blobs/uploads/") && req.Method == http.MethodPost:
		r.mu.Lock()
		id := fmt.Sprintf("u%d", len(r.uploads)+1)
		r.uploads[id] = nil
		r.mu.Unlock()
		// A single-shot POST carries the whole blob and its digest.
		if d := req.URL.Query().Get("digest"); d != "" {
			body, _ := io.ReadAll(req.Body)
			r.storeBlob(d, body)
			w.Header().Set("Docker-Content-Digest", d)
			w.WriteHeader(http.StatusCreated)
			return
		}
		w.Header().Set("Location", strings.TrimSuffix(path, "/")+"/"+id)
		w.Header().Set("Range", "0-0")
		w.Header().Set("Docker-Upload-UUID", id)
		w.WriteHeader(http.StatusAccepted)

	// Chunked upload, then finalize. Docker uses POST→PATCH→PUT; some clients go
	// straight to a monolithic PUT, so both accumulate the same way.
	case strings.Contains(path, "/blobs/uploads/") && (req.Method == http.MethodPatch || req.Method == http.MethodPut):
		id := path[strings.LastIndexByte(path, '/')+1:]
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.uploads[id] = append(r.uploads[id], body...)
		content := r.uploads[id]
		size := len(content)
		r.mu.Unlock()

		if req.Method == http.MethodPatch {
			w.Header().Set("Location", path)
			w.Header().Set("Range", fmt.Sprintf("0-%d", size-1))
			w.WriteHeader(http.StatusAccepted)
			return
		}
		// Finalize: the client names the digest it expects, and a registry that
		// stored something else would be silently corrupting the image.
		want := req.URL.Query().Get("digest")
		if got := digestOf(content); want != "" && got != want {
			http.Error(w, `{"errors":[{"code":"DIGEST_INVALID"}]}`, http.StatusBadRequest)
			return
		}
		r.storeBlob(want, content)
		w.Header().Set("Docker-Content-Digest", want)
		w.WriteHeader(http.StatusCreated)

	// Blob presence. Answering from what was actually stored is the difference
	// between the two Docker image stores working and only one of them working.
	case strings.Contains(path, "/blobs/") && (req.Method == http.MethodHead || req.Method == http.MethodGet):
		d := path[strings.LastIndexByte(path, '/')+1:]
		r.mu.Lock()
		content, ok := r.blobs[d]
		r.mu.Unlock()
		if !ok {
			http.Error(w, `{"errors":[{"code":"BLOB_UNKNOWN"}]}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
		w.Header().Set("Docker-Content-Digest", d)
		w.WriteHeader(http.StatusOK)
		if req.Method == http.MethodGet {
			_, _ = w.Write(content)
		}

	case strings.Contains(path, "/manifests/") && req.Method == http.MethodPut:
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.manifests = append(r.manifests, string(body))
		r.mu.Unlock()
		// The real digest of what we were handed: the client verifies this
		// against its own computation and rejects the push if they disagree.
		sum := sha256.Sum256(body)
		w.Header().Set("Docker-Content-Digest", "sha256:"+hex.EncodeToString(sum[:]))
		w.WriteHeader(http.StatusCreated)

	case strings.Contains(path, "/manifests/") && (req.Method == http.MethodHead || req.Method == http.MethodGet):
		http.Error(w, `{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`, http.StatusNotFound)

	default:
		w.WriteHeader(http.StatusOK)
	}
}

func (r *fakeRegistry) storeBlob(digest string, content []byte) {
	if digest == "" {
		digest = digestOf(content)
	}
	r.mu.Lock()
	r.blobs[digest] = content
	r.pushed++
	r.mu.Unlock()
}

func (r *fakeRegistry) counts() (manifests, blobs, anonymous int, auths []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.manifests), r.pushed, r.anonymous, append([]string(nil), r.seenAuth...)
}

// buildScratchImage builds a tiny local image with no base layer, so nothing has
// to be pulled from a registry we may not be able to reach.
func buildScratchImage(t *testing.T, docker *DockerClient, tag string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "payload.txt"), []byte("sigmahub push e2e\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dockerfile := "FROM scratch\nCOPY payload.txt /payload.txt\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := docker.ImageBuild(context.Background(), dir, "Dockerfile", tag, io.Discard); err != nil {
		t.Fatalf("build %s: %v", tag, err)
	}
}

// A push to a registry that requires credentials must actually send them. This
// is the regression: the credential was never attached, so the push 401'd and
// the image never reached anywhere the deploy target could pull from.
func TestDockerE2EAuthenticatedPush(t *testing.T) {
	if os.Getenv("SIGMAD_DOCKER_E2E") == "" {
		t.Skip("set SIGMAD_DOCKER_E2E=1 to run the real-Docker e2e")
	}
	docker := NewDockerClient("", os.Getenv("DOCKER_HOST"))
	if avail, _ := Probe(context.Background(), docker); !avail {
		t.Skip("docker daemon not reachable")
	}

	reg := newFakeRegistry("builder", "s3cret")
	// 127.0.0.1 so the daemon — which shares this process's loopback — treats it
	// as an insecure registry and speaks plain HTTP without any daemon config.
	srv := httptest.NewServer(reg)
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	tag := host + "/acme/res_e2e:abc-pin1"
	buildScratchImage(t, docker, tag)
	defer func() { _ = docker.ImageRemove(context.Background(), tag, true) }()

	err := docker.ImagePush(context.Background(), tag, build.RegistryAuth{
		Host: host, Username: "builder", Password: "s3cret",
	}, io.Discard)
	if err != nil {
		t.Fatalf("authenticated push failed: %v", err)
	}

	manifests, blobs, _, auths := reg.counts()
	if manifests == 0 {
		t.Fatal("the registry received no manifest — nothing was actually pushed")
	}
	if blobs == 0 {
		t.Fatal("the registry received no blobs")
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("builder:s3cret"))
	found := false
	for _, a := range auths {
		if a == want {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("the push never presented the registry credential; headers seen: %v", auths)
	}
}

// Without a credential the push must FAIL, and the failure must reach the op.
// A 401 swallowed under a 200-with-error-stream is how a "successful" build
// leaves the deploy target waiting on an image that never arrives.
func TestDockerE2EAnonymousPushIsRejected(t *testing.T) {
	if os.Getenv("SIGMAD_DOCKER_E2E") == "" {
		t.Skip("set SIGMAD_DOCKER_E2E=1 to run the real-Docker e2e")
	}
	docker := NewDockerClient("", os.Getenv("DOCKER_HOST"))
	if avail, _ := Probe(context.Background(), docker); !avail {
		t.Skip("docker daemon not reachable")
	}

	reg := newFakeRegistry("builder", "s3cret")
	srv := httptest.NewServer(reg)
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	tag := host + "/acme/res_anon:abc"
	buildScratchImage(t, docker, tag)
	defer func() { _ = docker.ImageRemove(context.Background(), tag, true) }()

	err := docker.ImagePush(context.Background(), tag, build.RegistryAuth{}, io.Discard)
	if err == nil {
		t.Fatal("an anonymous push to a registry that requires auth must fail the op")
	}
	if manifests, _, _, _ := reg.counts(); manifests != 0 {
		t.Fatal("a rejected push must not have delivered a manifest")
	}
}
