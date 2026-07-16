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
// independently so a repo with only some of them still resolves.
var candidateFiles = []string{
	"Dockerfile", "dockerfile",
	"compose.yaml", "compose.yml", "docker-compose.yml", "docker-compose.yaml",
}

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
	return gitdetect.Detect(files), nil
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
		return nil, false, fmt.Errorf("github contents %s: %s (check installation token/permissions)", path, resp.Status)
	default:
		return nil, false, fmt.Errorf("github contents %s: unexpected status %s", path, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, false, err
	}
	var cr contentsResponse
	if err := json.Unmarshal(body, &cr); err != nil {
		return nil, false, fmt.Errorf("github contents %s: decode: %w", path, err)
	}
	if cr.Type != "file" {
		return nil, false, nil
	}
	if cr.Encoding != "base64" {
		return nil, false, fmt.Errorf("github contents %s: unexpected encoding %q", path, cr.Encoding)
	}
	// The API wraps base64 content at 60 cols with newlines.
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(cr.Content, "\n", ""))
	if err != nil {
		return nil, false, fmt.Errorf("github contents %s: base64: %w", path, err)
	}
	return decoded, true, nil
}
