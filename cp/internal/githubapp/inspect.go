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

// candidateFiles are the root files the detector understands; each is fetched
// independently so a repo with only some of them still resolves. Env templates
// (.env.example & co) contribute variable KEYS to the wizard pre-fill and never
// affect deployability.
var candidateFiles = append([]string{
	"Dockerfile", "dockerfile",
	"compose.yaml", "compose.yml", "docker-compose.yml", "docker-compose.yaml",
}, gitdetect.EnvExampleNames...)

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

	files := map[string][]byte{}
	for _, name := range candidateFiles {
		content, found, err := i.fetchFile(ctx, base, repo, name, token)
		if err != nil {
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
