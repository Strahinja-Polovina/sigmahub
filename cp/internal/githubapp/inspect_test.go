package githubapp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubContents serves the GitHub Contents API for a fixed file set.
func stubContents(files map[string]string, wantAuth string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if wantAuth != "" && r.Header.Get("Authorization") != wantAuth {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// /repos/{owner}/{name}/contents/{path}
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/repos/"), "/contents/", 2)
		if len(parts) != 2 {
			// Repo-level probe (GET /repos/{owner}/{name}): the repo exists in
			// these fixtures; only its files vary.
			_ = json.NewEncoder(w).Encode(map[string]string{"full_name": parts[0], "default_branch": "main"})
			return
		}
		content, ok := files[parts[1]]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// The real API wraps base64 in 60-col lines; emulate a newline.
		enc := base64.StdEncoding.EncodeToString([]byte(content))
		_ = json.NewEncoder(w).Encode(map[string]string{
			"type": "file", "encoding": "base64", "content": enc[:len(enc)/2] + "\n" + enc[len(enc)/2:],
		})
	}))
}

func TestInspectDockerfile(t *testing.T) {
	srv := stubContents(map[string]string{
		"Dockerfile": "FROM node:20\nEXPOSE 3000\nENV PORT=3000\n",
	}, "")
	defer srv.Close()

	insp := &Inspector{Client: srv.Client(), APIBase: srv.URL}
	d, err := insp.Inspect(context.Background(), "owner/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	if !d.HasDockerfile || !d.Deployable {
		t.Fatalf("expected deployable Dockerfile detection, got %+v", d)
	}
	if len(d.Ports) != 1 || d.Ports[0] != 3000 {
		t.Errorf("ports = %v, want [3000]", d.Ports)
	}
}

func TestInspectAuthForwarded(t *testing.T) {
	srv := stubContents(map[string]string{
		"compose.yaml": "services:\n  a:\n    ports:\n      - \"80:80\"\n",
	}, "Bearer tok123")
	defer srv.Close()

	insp := &Inspector{Client: srv.Client(), APIBase: srv.URL}
	// Without the token the repo is invisible → an actionable non-deployable
	// result (NOT an opaque error) so the UI can say "connect it with a token".
	if d, err := insp.Inspect(context.Background(), "o/r", ""); err != nil {
		t.Fatalf("tokenless inspect of an invisible repo must not error: %v", err)
	} else if d.Deployable || !strings.Contains(d.Reason, "not accessible") {
		t.Errorf("expected not-accessible result without token, got %+v", d)
	}
	// With the token → detection succeeds.
	d, err := insp.Inspect(context.Background(), "o/r", "tok123")
	if err != nil {
		t.Fatal(err)
	}
	if !d.HasCompose {
		t.Errorf("expected compose detection, got %+v", d)
	}
}

func TestInspectUndeployable(t *testing.T) {
	srv := stubContents(map[string]string{"README.md": "# hi"}, "")
	defer srv.Close()

	insp := &Inspector{Client: srv.Client(), APIBase: srv.URL}
	d, err := insp.Inspect(context.Background(), "o/r", "")
	if err != nil {
		t.Fatal(err)
	}
	if d.Deployable {
		t.Fatal("repo with no Dockerfile/compose must be undeployable")
	}
	if d.Reason == "" {
		t.Error("expected actionable reason")
	}
}

// TestInspectInaccessibleRepo proves an invisible repo (GitHub 404s every path
// of a private repo when the token is missing/unauthorized) is reported as
// not-accessible — not as the misleading "no Dockerfile found".
func TestInspectInaccessibleRepo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	insp := &Inspector{Client: srv.Client(), APIBase: srv.URL}
	d, err := insp.Inspect(context.Background(), "o/private", "")
	if err != nil {
		t.Fatal(err)
	}
	if d.Deployable {
		t.Fatal("an invisible repo must not be deployable")
	}
	if !strings.Contains(d.Reason, "not accessible") {
		t.Errorf("reason should say the repo is not accessible, got %q", d.Reason)
	}
}

func TestInspectBadRepo(t *testing.T) {
	insp := NewInspector()
	if _, err := insp.Inspect(context.Background(), "no-slash", ""); err == nil {
		t.Error("expected error for malformed repo name")
	}
}

// TestInspectSkipsOversizeCandidate proves a candidate GitHub returns with
// encoding "none" (too large to inline) is skipped, not fatal — a valid
// Dockerfile in the same repo is still detected.
func TestInspectSkipsOversizeCandidate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/contents/") {
			_ = json.NewEncoder(w).Encode(map[string]string{"default_branch": "main"})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/contents/Dockerfile") {
			enc := base64.StdEncoding.EncodeToString([]byte("FROM x\nEXPOSE 3000\n"))
			_ = json.NewEncoder(w).Encode(map[string]string{"type": "file", "encoding": "base64", "content": enc})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/contents/docker-compose.yml") {
			// Too large to inline → GitHub returns encoding "none", empty content.
			_ = json.NewEncoder(w).Encode(map[string]string{"type": "file", "encoding": "none", "content": ""})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	insp := &Inspector{Client: srv.Client(), APIBase: srv.URL}
	d, err := insp.Inspect(context.Background(), "o/r", "")
	if err != nil {
		t.Fatalf("a skippable candidate must not abort detection: %v", err)
	}
	if !d.HasDockerfile || len(d.Ports) != 1 || d.Ports[0] != 3000 {
		t.Errorf("Dockerfile should still be detected: %+v", d)
	}
}
