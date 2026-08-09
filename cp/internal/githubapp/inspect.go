// Package githubapp holds the outbound GitHub integration: reading a connected
// repository's root files through the Contents API so the connect wizard can
// pre-fill the detected deploy configuration.
package githubapp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/gitdetect"
)

// DefaultAPIBase is the public GitHub REST API root.
const DefaultAPIBase = "https://api.github.com"

// Inspector reads a repo's root files via the GitHub Contents API and derives
// the deploy configuration. A zero token inspects a public repo unauthenticated.
type Inspector struct {
	Client  *http.Client
	APIBase string
}

// NewInspector returns an Inspector with a bounded HTTP client.
func NewInspector() *Inspector {
	return &Inspector{
		Client:  &http.Client{Timeout: 10 * time.Second},
		APIBase: DefaultAPIBase,
	}
}

// maxDetectDepth caps how deep a build may be found. Two levels covers the
// shapes people actually have (apps/api, services/web, src) without turning the
// inspection of a large monorepo into hundreds of Contents calls.
const maxDetectDepth = 2

// maxDetectFiles caps how many files one inspection fetches. A repository that
// legitimately contains fifty package.json files is a workspace, not fifty
// deployables, and reading all of them would spend the installation's rate
// limit on a pre-fill.
const maxDetectFiles = 40

// Inspect fetches the candidate root files and runs detection. Missing files are
// skipped (a 404 is normal); a transport or unexpected-status error aborts. The
// returned Detected is never partial-on-error — callers get either a full result
// or an error.
func (i *Inspector) Inspect(ctx context.Context, repoFullName, token string) (gitdetect.Detected, error) {
	repo := strings.Trim(strings.TrimSpace(repoFullName), "/")
	if !strings.Contains(repo, "/") {
		return gitdetect.Detected{}, fmt.Errorf("repo must be owner/name, got %q", repoFullName)
	}
	base := i.APIBase
	if base == "" {
		base = DefaultAPIBase
	}

	// Repo-level probe FIRST: it distinguishes "no Dockerfile" from "repo
	// invisible" (GitHub 404s every path of a private repo when the token is
	// missing/unauthorized) and yields the default branch for the wizard's
	// auto branch-mapping.
	visible, defaultBranch, err := i.repoMeta(ctx, base, repo, token)
	if err != nil {
		return gitdetect.Detected{}, err
	}
	if !visible {
		return gitdetect.Detected{
			Ports: []int{}, Env: []string{},
			HealthCheck: gitdetect.HealthCheck{Type: "tcp", IntervalSec: 10, Source: "default"},
			Deployable:  false,
			Reason: "repository not found or not accessible — if it is private, connect it with an access token " +
				"or GitHub App and try again",
		}, nil
	}

	// ONE listing call decides what is worth reading, instead of guessing paths.
	// Guessing is what limited detection to the repository root: probing
	// "apps/api/Dockerfile" for every plausible directory would have cost dozens
	// of Contents calls per inspection, so nobody did, so a monorepo was
	// undeployable. The tree endpoint answers the same question once.
	wanted, listed := i.wantedPaths(ctx, base, repo, defaultBranch, token)
	if !listed {
		// No listing (an empty repo, a 409 on an unborn branch, a permission
		// shape that allows contents but not trees): fall back to probing the
		// conventional paths. Worse, but never worse than root-only was.
		//
		// Capped like the listing path is. The candidate set is ~260 paths and
		// each is a separate sequential request inside the call the user is
		// watching a spinner on — uncapped, the fallback for a repo we cannot
		// list turns a slow screen into a multi-minute hang. The set is ordered
		// root-first, so the cap keeps the paths most likely to matter.
		wanted = gitdetect.CandidatePaths()
		if len(wanted) > maxDetectFiles {
			wanted = wanted[:maxDetectFiles]
		}
	}

	files := map[string][]byte{}
	for _, name := range wanted {
		content, found, err := i.fetchFile(ctx, base, repo, name, token)
		if err != nil {
			// A missing path in the FALLBACK set is expected; only a real
			// listing promises the file exists, so only that aborts.
			if !listed {
				continue
			}
			return gitdetect.Detected{}, err
		}
		if found {
			files[name] = content
		}
	}
	d := gitdetect.Detect(files)
	d.DefaultBranch = defaultBranch
	return d, nil
}

// wantedPaths lists the repo tree once and returns the detection-relevant
// subset. ok=false means the tree could not be read and the caller should probe
// conventional paths instead — never an error, because a pre-fill that can
// still be produced must not be turned into a failed inspection.
func (i *Inspector) wantedPaths(ctx context.Context, base, repo, ref, token string) (paths []string, ok bool) {
	if ref == "" {
		ref = "HEAD"
	}
	u := fmt.Sprintf("%s/repos/%s/git/trees/%s?recursive=1", base, repo, url.PathEscape(ref))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, false
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := i.Client.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, false
	}
	var tree struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"tree"`
	}
	// A 200 that carries no tree array is not a listing — decode succeeds on any
	// JSON object, so without this an unexpected-but-successful response would
	// read as "this repository contains no files" and detect nothing at all,
	// instead of falling through to probing.
	if err := json.Unmarshal(body, &tree); err != nil || tree.Tree == nil {
		return nil, false
	}
	all := make([]string, 0, len(tree.Tree))
	for _, e := range tree.Tree {
		if e.Type != "blob" {
			continue
		}
		all = append(all, e.Path)
	}
	// A truncated tree is still a tree: GitHub truncates the TAIL, and every
	// path we care about is shallow, so the head of the listing is exactly the
	// part that survives. Using it beats falling back to guessing.
	wanted := gitdetect.WantedPaths(all, maxDetectDepth)
	if len(wanted) > maxDetectFiles {
		wanted = wanted[:maxDetectFiles]
	}
	return wanted, true
}

// BranchHead returns the branch's head commit sha (GET /repos/{r}/branches/{b})
// — the input for an initial deploy that must not wait for a webhook push.
func (i *Inspector) BranchHead(ctx context.Context, repoFullName, branch, token string) (string, error) {
	repo := strings.Trim(strings.TrimSpace(repoFullName), "/")
	base := i.APIBase
	if base == "" {
		base = DefaultAPIBase
	}
	u := fmt.Sprintf("%s/repos/%s/branches/%s", base, repo, branch)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := i.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return "", fmt.Errorf("github branch %s@%s: status %s", repo, branch, resp.Status)
	}
	var out struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	if out.Commit.SHA == "" {
		return "", fmt.Errorf("github branch %s@%s: no head commit in response", repo, branch)
	}
	return out.Commit.SHA, nil
}

// RegisterPushWebhook idempotently creates the push/pull_request webhook that
// drives push-to-deploy (POST /repos/{r}/hooks). GitHub answers 422 when an
// identical hook already exists — treated as success. Requires a token with
// webhook (admin:repo_hook) permission.
func (i *Inspector) RegisterPushWebhook(ctx context.Context, repoFullName, hookURL, secret, token string) error {
	repo := strings.Trim(strings.TrimSpace(repoFullName), "/")
	base := i.APIBase
	if base == "" {
		base = DefaultAPIBase
	}
	payload, err := json.Marshal(map[string]any{
		"name":   "web",
		"active": true,
		"events": []string{"push", "pull_request"},
		"config": map[string]string{
			"url":          hookURL,
			"content_type": "json",
			"secret":       secret,
		},
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/repos/%s/hooks", base, repo), strings.NewReader(string(payload)))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := i.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode == http.StatusCreated:
		return nil
	case resp.StatusCode == http.StatusUnprocessableEntity && strings.Contains(string(body), "already exists"):
		return nil
	default:
		return fmt.Errorf("github create webhook %s: status %s (the token needs webhook/admin:repo_hook permission)", repo, resp.Status)
	}
}

// repoMeta resolves the repo itself (GET /repos/{owner}/{name}): whether it is
// visible with the given credentials, and its default branch. A 404 means the
// repo does not exist or is private and unreadable with this token.
func (i *Inspector) repoMeta(ctx context.Context, base, repo, token string) (visible bool, defaultBranch string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/repos/%s", base, repo), nil)
	if err != nil {
		return false, "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := i.Client.Do(req)
	if err != nil {
		return false, "", err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		var meta struct {
			DefaultBranch string `json:"default_branch"`
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			return false, "", err
		}
		if err := json.Unmarshal(body, &meta); err != nil {
			// Metadata decode trouble must not block detection; branch is a nicety.
			return true, "", nil
		}
		return true, meta.DefaultBranch, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		// GitHub also 403s here when the UNAUTHENTICATED rate limit is spent —
		// that failure has a fix the operator should hear about.
		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			return false, "", fmt.Errorf(
				"github API rate limit exceeded — connect the repo with an access token or GitHub App (much higher limit), or retry later")
		}
		return false, "", nil
	case http.StatusNotFound:
		return false, "", nil
	default:
		return false, "", fmt.Errorf("github repo %s: unexpected status %s", repo, resp.Status)
	}
}

type contentsResponse struct {
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
	Type     string `json:"type"`
}

func (i *Inspector) fetchFile(ctx context.Context, base, repo, path, token string) ([]byte, bool, error) {
	url := fmt.Sprintf("%s/repos/%s/contents/%s", base, repo, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := i.Client.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		// proceed
	case http.StatusNotFound:
		return nil, false, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		// GitHub uses 403 both for permissions AND for the (60/hr per IP)
		// unauthenticated rate limit — tell the operator which one it was.
		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			return nil, false, fmt.Errorf(
				"github API rate limit exceeded reading %s — connect the repo with an access token or GitHub App (much higher limit), or retry later", path)
		}
		return nil, false, fmt.Errorf("github contents %s: %s (check installation token/permissions)", path, resp.Status)
	default:
		return nil, false, fmt.Errorf("github contents %s: unexpected status %s", path, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, false, err
	}
	var cr contentsResponse
	if err := json.Unmarshal(body, &cr); err != nil {
		return nil, false, fmt.Errorf("github contents %s: decode: %w", path, err)
	}
	// A per-file condition that isn't a base64 file — a directory/submodule, or a
	// file too large to inline (GitHub returns encoding "none" above ~1 MB) — is
	// SKIPPED, not fatal: one odd candidate must not abort detection of the rest.
	if cr.Type != "file" || cr.Encoding != "base64" {
		return nil, false, nil
	}
	// The API wraps base64 content at 60 cols with newlines.
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(cr.Content, "\n", ""))
	if err != nil {
		return nil, false, nil
	}
	return decoded, true, nil
}
