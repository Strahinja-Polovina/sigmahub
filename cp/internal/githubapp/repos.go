package githubapp

// Listing the repositories an installation can reach. This is what turns GitHub
// from a per-repo connect form into an org-level integration: connect the App
// once, then PICK a repo from what it already grants.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// Repo is one selectable repository.
type Repo struct {
	FullName      string `json:"fullName"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"defaultBranch"`
	// InstallationID is filled by the caller when merging several installations,
	// so the picker knows which credential reads a given repo.
	InstallationID string `json:"installationId,omitempty"`
}

// repoPageSize is GitHub's maximum per_page for this endpoint.
const repoPageSize = 100

// maxRepoPages bounds the walk. An installation with more than 1000 repos is
// beyond what a picker list is usable for anyway, and the cap keeps one API
// call from turning into an unbounded fan-out; the caller surfaces truncation.
const maxRepoPages = 10

// ListInstallationRepos returns the repositories the installation token grants,
// paginating until GitHub runs out or the page cap is hit. truncated reports
// that the cap stopped the walk, so callers can say so rather than silently
// presenting a partial list as complete.
func (i *Inspector) ListInstallationRepos(ctx context.Context, token string) (repos []Repo, truncated bool, err error) {
	if token == "" {
		return nil, false, fmt.Errorf("an installation token is required to list repositories")
	}
	base := i.APIBase
	if base == "" {
		base = DefaultAPIBase
	}

	for page := 1; page <= maxRepoPages; page++ {
		u := fmt.Sprintf("%s/installation/repositories?per_page=%d&page=%d",
			base, repoPageSize, page)
		batch, total, err := i.fetchRepoPage(ctx, u, token)
		if err != nil {
			return nil, false, err
		}
		repos = append(repos, batch...)
		// Done when this page didn't fill, or we've seen everything GitHub counted.
		if len(batch) < repoPageSize || (total > 0 && len(repos) >= total) {
			return repos, false, nil
		}
	}
	return repos, true, nil
}

func (i *Inspector) fetchRepoPage(ctx context.Context, endpoint, token string) ([]Repo, int, error) {
	if _, err := url.Parse(endpoint); err != nil {
		return nil, 0, fmt.Errorf("build repositories url: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+token)

	client := i.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// Bounded read: an error body is for the message, not for parsing.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, 0, fmt.Errorf("list installation repositories: %s: %s",
			resp.Status, string(snippet))
	}

	var payload struct {
		TotalCount   int `json:"total_count"`
		Repositories []struct {
			FullName      string `json:"full_name"`
			Private       bool   `json:"private"`
			DefaultBranch string `json:"default_branch"`
		} `json:"repositories"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxRepoListBytes)).Decode(&payload); err != nil {
		return nil, 0, fmt.Errorf("decode repositories: %w", err)
	}
	out := make([]Repo, 0, len(payload.Repositories))
	for _, r := range payload.Repositories {
		if r.FullName == "" {
			continue
		}
		out = append(out, Repo{
			FullName:      r.FullName,
			Private:       r.Private,
			DefaultBranch: r.DefaultBranch,
		})
	}
	return out, payload.TotalCount, nil
}

// maxRepoListBytes bounds one page of JSON (100 repos is well under this).
const maxRepoListBytes = 4 << 20
