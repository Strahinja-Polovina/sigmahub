package githubapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// repoServer serves /installation/repositories from a fixed repo set, honouring
// per_page/page so pagination is exercised for real.
func repoServer(t *testing.T, total int, wantAuth string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/installation/repositories") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != wantAuth {
			t.Errorf("Authorization = %q, want %q", got, wantAuth)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		page := 1
		if p := r.URL.Query().Get("page"); p != "" {
			_, _ = fmt.Sscanf(p, "%d", &page)
		}
		perPage := repoPageSize
		start := (page - 1) * perPage
		type repo struct {
			FullName      string `json:"full_name"`
			Private       bool   `json:"private"`
			DefaultBranch string `json:"default_branch"`
		}
		out := []repo{}
		for i := start; i < total && i < start+perPage; i++ {
			out = append(out, repo{
				FullName:      fmt.Sprintf("acme/repo-%03d", i),
				Private:       i%2 == 0,
				DefaultBranch: "main",
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"total_count":  total,
			"repositories": out,
		})
	}))
}

func TestListInstallationReposSinglePage(t *testing.T) {
	srv := repoServer(t, 3, "Bearer tok")
	defer srv.Close()
	i := &Inspector{Client: srv.Client(), APIBase: srv.URL}

	repos, truncated, err := i.ListInstallationRepos(context.Background(), "tok")
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("a short list must not report truncation")
	}
	if len(repos) != 3 {
		t.Fatalf("repos = %d, want 3", len(repos))
	}
	if repos[0].FullName != "acme/repo-000" || repos[0].DefaultBranch != "main" {
		t.Fatalf("first repo = %+v", repos[0])
	}
	if !repos[0].Private || repos[1].Private {
		t.Fatalf("visibility not carried through: %+v", repos[:2])
	}
}

func TestListInstallationReposPaginates(t *testing.T) {
	// 150 repos spans two pages; the walk must return all of them exactly once.
	srv := repoServer(t, 150, "Bearer tok")
	defer srv.Close()
	i := &Inspector{Client: srv.Client(), APIBase: srv.URL}

	repos, truncated, err := i.ListInstallationRepos(context.Background(), "tok")
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("150 repos fits well inside the page cap")
	}
	if len(repos) != 150 {
		t.Fatalf("repos = %d, want 150", len(repos))
	}
	seen := map[string]bool{}
	for _, r := range repos {
		if seen[r.FullName] {
			t.Fatalf("duplicate repo across pages: %s", r.FullName)
		}
		seen[r.FullName] = true
	}
}

func TestListInstallationReposRequiresToken(t *testing.T) {
	i := NewInspector()
	if _, _, err := i.ListInstallationRepos(context.Background(), ""); err == nil {
		t.Fatal("an empty token must be refused, not sent unauthenticated")
	}
}

func TestListInstallationReposSurfacesAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	defer srv.Close()
	i := &Inspector{Client: srv.Client(), APIBase: srv.URL}

	_, _, err := i.ListInstallationRepos(context.Background(), "tok")
	if err == nil {
		t.Fatal("a 403 must be an error, not an empty repo list")
	}
	// The reason has to reach the operator — an empty picker with no explanation
	// is exactly the failure this endpoint exists to avoid.
	if !strings.Contains(err.Error(), "Bad credentials") {
		t.Fatalf("error must carry GitHub's reason, got %v", err)
	}
}
