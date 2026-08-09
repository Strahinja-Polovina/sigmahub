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

// stubContents serves the GitHub Contents API — and the Git Trees API, which is
// how the inspector learns a repository has anything below its root — for a
// fixed file set. serveTree=false emulates a repo whose tree cannot be read, so
// the conventional-path fallback is exercised rather than assumed.
func stubContents(files map[string]string, wantAuth string) *httptest.Server {
	return stubRepo(files, wantAuth, true)
}

func stubRepo(files map[string]string, wantAuth string, serveTree bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if wantAuth != "" && r.Header.Get("Authorization") != wantAuth {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if strings.Contains(r.URL.Path, "/git/trees/") {
			if !serveTree {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			entries := make([]map[string]string, 0, len(files))
			for p := range files {
				entries = append(entries, map[string]string{"path": p, "type": "blob"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"tree": entries})
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

// A monorepo: nothing at the root, the application two directories down. Every
// path of this shape used to come back "not deployable", because the inspector
// only ever asked about the root — the fix is a tree listing, so this drives the
// listing rather than the detector.
func TestInspectFindsBuildBelowTheRoot(t *testing.T) {
	srv := stubContents(map[string]string{
		"README.md":             "# monorepo",
		"pnpm-workspace.yaml":   "packages:\n  - apps/*\n",
		"apps/api/Dockerfile":   "FROM golang:1.24\nEXPOSE 8080\nENV DATABASE_URL=x\n",
		"apps/api/go.mod":       "module acme/api\n",
		"apps/web/package.json": `{"name":"web"}`,
	}, "")
	defer srv.Close()

	insp := &Inspector{Client: srv.Client(), APIBase: srv.URL}
	d, err := insp.Inspect(context.Background(), "acme/monorepo", "")
	if err != nil {
		t.Fatal(err)
	}
	if !d.Deployable || !d.HasDockerfile {
		t.Fatalf("monorepo with apps/api/Dockerfile must be deployable: %+v", d)
	}
	if d.ContextSubdir != "apps/api" {
		t.Errorf("context subdir = %q, want apps/api", d.ContextSubdir)
	}
	// The path handed to the build op is relative to the CONTEXT: the pair
	// (apps/api, Dockerfile) is what the agent can act on, (., apps/api/...) is
	// what it would look for in the wrong place.
	if d.DockerfileT != "Dockerfile" {
		t.Errorf("dockerfile path = %q, want Dockerfile (relative to the context)", d.DockerfileT)
	}
	if len(d.Ports) != 1 || d.Ports[0] != 8080 {
		t.Errorf("ports = %v, want [8080] from the subdirectory Dockerfile", d.Ports)
	}
}

// A repository that describes no build at all, but plainly IS a Go service.
// This is the nixpacks fallback reaching the user through the real inspector:
// the go.mod has to be fetched, which only happens because it is in the wanted
// set — the previous candidate list held six file names and none of them
// identified a language.
func TestInspectFallsBackToNixpacksLanguage(t *testing.T) {
	srv := stubContents(map[string]string{
		"go.mod":       "module acme/reporting\n",
		"main.go":      "package main\n",
		".env.example": "REPORT_BUCKET=\nLOG_LEVEL=info\n",
	}, "")
	defer srv.Close()

	insp := &Inspector{Client: srv.Client(), APIBase: srv.URL}
	d, err := insp.Inspect(context.Background(), "acme/reporting", "")
	if err != nil {
		t.Fatal(err)
	}
	if !d.Deployable {
		t.Fatalf("a Go service with no Dockerfile must not be a dead end: %+v", d)
	}
	if d.BuildMethod != "nixpacks" || d.Language != "go" {
		t.Errorf("build method/language = %q/%q, want nixpacks/go", d.BuildMethod, d.Language)
	}
	// The env template still seeds the Variables step — it is beside the build.
	if len(d.Env) != 2 {
		t.Errorf("env keys = %v, want the two from .env.example", d.Env)
	}
}

// When the tree cannot be read the inspector probes conventional paths instead
// of giving up. Root detection has to keep working through that path, because
// it is what an empty-ish or permission-limited repo actually hits.
func TestInspectFallsBackToProbingWhenTreeUnavailable(t *testing.T) {
	srv := stubRepo(map[string]string{
		"Dockerfile": "FROM x\nEXPOSE 3000\n",
	}, "", false)
	defer srv.Close()

	insp := &Inspector{Client: srv.Client(), APIBase: srv.URL}
	d, err := insp.Inspect(context.Background(), "o/r", "")
	if err != nil {
		t.Fatal(err)
	}
	if !d.HasDockerfile || len(d.Ports) != 1 || d.Ports[0] != 3000 {
		t.Errorf("probing fallback lost the root Dockerfile: %+v", d)
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
		if strings.Contains(r.URL.Path, "/git/trees/") {
			_ = json.NewEncoder(w).Encode(map[string]any{"tree": []map[string]string{
				{"path": "Dockerfile", "type": "blob"},
				{"path": "docker-compose.yml", "type": "blob"},
			}})
			return
		}
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
