package integration

// A registry for the fleet e2e.
//
// The deploy pipeline pulls: `image.pull` runs against whatever the spec names,
// and a locally-tagged image resolves to Docker Hub and fails. Pointing the
// fixture at a real registry instead is not a workaround — it is the path the
// product actually takes for every cross-host image, so the fleet test covers
// the round trip rather than stepping around it.
//
// Anonymous on purpose: the credential half of this is already covered against
// a real `docker push` in the agent's own e2e. What matters here is that an
// image published by one step is pullable by another.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

type fleetRegistry struct {
	mu      sync.Mutex
	uploads map[string][]byte
	blobs   map[string][]byte
	// manifests is keyed by BOTH tag and digest: a pull asks by tag, and the
	// client then verifies the digest it was told.
	manifests map[string]fleetManifest
}

type fleetManifest struct {
	body        []byte
	contentType string
	digest      string
}

func newFleetRegistry() *fleetRegistry {
	return &fleetRegistry{
		uploads:   map[string][]byte{},
		blobs:     map[string][]byte{},
		manifests: map[string]fleetManifest{},
	}
}

func fleetDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ref splits /v2/<name>/<kind>/<reference> into its last segment.
func lastSegment(path string) string { return path[strings.LastIndexByte(path, '/')+1:] }

func (r *fleetRegistry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	path := req.URL.Path

	switch {
	case path == "/v2" || path == "/v2/":
		w.WriteHeader(http.StatusOK)

	case strings.HasSuffix(path, "/blobs/uploads/") && req.Method == http.MethodPost:
		r.mu.Lock()
		id := fmt.Sprintf("u%d", len(r.uploads)+1)
		r.uploads[id] = nil
		r.mu.Unlock()
		if d := req.URL.Query().Get("digest"); d != "" {
			body, _ := io.ReadAll(req.Body)
			r.put(d, body)
			w.Header().Set("Docker-Content-Digest", d)
			w.WriteHeader(http.StatusCreated)
			return
		}
		w.Header().Set("Location", strings.TrimSuffix(path, "/")+"/"+id)
		w.Header().Set("Range", "0-0")
		w.WriteHeader(http.StatusAccepted)

	case strings.Contains(path, "/blobs/uploads/") && (req.Method == http.MethodPatch || req.Method == http.MethodPut):
		id := lastSegment(path)
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.uploads[id] = append(r.uploads[id], body...)
		content := r.uploads[id]
		r.mu.Unlock()
		if req.Method == http.MethodPatch {
			w.Header().Set("Location", path)
			w.Header().Set("Range", fmt.Sprintf("0-%d", len(content)-1))
			w.WriteHeader(http.StatusAccepted)
			return
		}
		digest := req.URL.Query().Get("digest")
		if digest == "" {
			digest = fleetDigest(content)
		}
		r.put(digest, content)
		w.Header().Set("Docker-Content-Digest", digest)
		w.WriteHeader(http.StatusCreated)

	case strings.Contains(path, "/blobs/"):
		d := lastSegment(path)
		r.mu.Lock()
		content, ok := r.blobs[d]
		r.mu.Unlock()
		if !ok {
			http.Error(w, `{"errors":[{"code":"BLOB_UNKNOWN"}]}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Docker-Content-Digest", d)
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
		w.WriteHeader(http.StatusOK)
		if req.Method == http.MethodGet {
			_, _ = w.Write(content)
		}

	case strings.Contains(path, "/manifests/") && req.Method == http.MethodPut:
		body, _ := io.ReadAll(req.Body)
		m := fleetManifest{body: body, contentType: req.Header.Get("Content-Type"), digest: fleetDigest(body)}
		if m.contentType == "" {
			m.contentType = "application/vnd.oci.image.manifest.v1+json"
		}
		r.mu.Lock()
		// Both keys, because a push names a tag and a pull may ask for either.
		r.manifests[lastSegment(path)] = m
		r.manifests[m.digest] = m
		r.mu.Unlock()
		w.Header().Set("Docker-Content-Digest", m.digest)
		w.WriteHeader(http.StatusCreated)

	case strings.Contains(path, "/manifests/"):
		r.mu.Lock()
		m, ok := r.manifests[lastSegment(path)]
		r.mu.Unlock()
		if !ok {
			http.Error(w, `{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", m.contentType)
		w.Header().Set("Docker-Content-Digest", m.digest)
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(m.body)))
		w.WriteHeader(http.StatusOK)
		if req.Method == http.MethodGet {
			_, _ = w.Write(m.body)
		}

	default:
		http.Error(w, `{"errors":[{"code":"UNSUPPORTED"}]}`, http.StatusNotFound)
	}
}

func (r *fleetRegistry) put(digest string, content []byte) {
	r.mu.Lock()
	r.blobs[digest] = content
	r.mu.Unlock()
}

// publishIdleImage builds the idle image and pushes it to a registry started for
// this test, returning the reference the agents will pull.
func publishIdleImage(t *testing.T) string {
	t.Helper()
	buildIdleImage(t)
	return publishImage(t, idleImage, "fleet/idle:1")
}

// publishCrashImage does the same for the container that dies on boot.
func publishCrashImage(t *testing.T) string {
	t.Helper()
	buildCrashImage(t)
	return publishImage(t, crashImage, "fleet/crash:1")
}

// publishImage pushes a locally-built tag to a registry started for this test
// and returns the reference the agents will pull.
//
// 127.0.0.1 so the daemon treats it as insecure and speaks plain HTTP without
// any daemon configuration.
func publishImage(t *testing.T, local, name string) string {
	t.Helper()
	srv := httptest.NewServer(newFleetRegistry())
	t.Cleanup(srv.Close)
	ref := strings.TrimPrefix(srv.URL, "http://") + "/" + name

	if out, err := exec.Command("docker", "tag", local, ref).CombinedOutput(); err != nil {
		t.Fatalf("tag %s: %v\n%s", ref, err, out)
	}
	if out, err := exec.Command("docker", "push", ref).CombinedOutput(); err != nil {
		t.Fatalf("push %s: %v\n%s", ref, err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rmi", "-f", ref).Run() })
	return ref
}
