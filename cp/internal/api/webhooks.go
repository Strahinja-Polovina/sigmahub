package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// zeroSHA is git's all-zero object id, sent as `after` on a branch delete.
const zeroSHA = "0000000000000000000000000000000000000000"

// maxWebhookBytes caps a webhook body. GitHub push payloads embed the full
// commits array and can legitimately exceed the 1 MiB default used for small
// authenticated posts, so a large merge push isn't silently dropped; GitHub's
// own delivery ceiling is 25 MiB.
const maxWebhookBytes = 25 << 20

// handleGitHubWebhook is the public, unauthenticated webhook receiver. Trust is
// established purely by the HMAC-SHA256 signature over the raw body: a forged or
// unsigned delivery is rejected before any parsing or state change. Processing
// is idempotent (deduped on the delivery id in the store), and the endpoint
// always answers 2xx on a valid signature so GitHub does not enter a redelivery
// storm — the outcome is conveyed in the body, not the status code.
func (s *Server) handleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	if s.git == nil || s.githubWebhookSecret == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "webhook receiver is not configured"})
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBytes))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unreadable body"})
		return
	}

	// Verify the signature over the EXACT bytes received, before trusting any
	// field. Constant-time compare; a missing/malformed/forged header is 401.
	if !validGitHubSignature(s.githubWebhookSecret, body, r.Header.Get("X-Hub-Signature-256")) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid signature"})
		return
	}

	deliveryID := strings.TrimSpace(r.Header.Get("X-GitHub-Delivery"))
	event := strings.TrimSpace(r.Header.Get("X-GitHub-Event"))
	if deliveryID == "" || event == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing X-GitHub-Delivery or X-GitHub-Event"})
		return
	}
	// The App's connectivity check: acknowledge without routing.
	if event == "ping" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "pong"})
		return
	}

	ev := store.GitWebhookEvent{DeliveryID: deliveryID, Provider: "github", EventType: event}
	switch event {
	case "push":
		var p struct {
			Ref        string `json:"ref"`
			After      string `json:"after"`
			Deleted    bool   `json:"deleted"`
			Repository struct {
				FullName string `json:"full_name"`
			} `json:"repository"`
		}
		if err := json.Unmarshal(body, &p); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid push payload"})
			return
		}
		ev.Ref = p.Ref
		ev.SHA = p.After
		ev.RepoFullName = p.Repository.FullName
		ev.Deleted = p.Deleted || p.After == zeroSHA
	case "pull_request":
		var p struct {
			Action      string `json:"action"`
			Number      int    `json:"number"`
			PullRequest struct {
				Head struct {
					Ref  string `json:"ref"`
					SHA  string `json:"sha"`
					Repo struct {
						FullName string `json:"full_name"`
					} `json:"repo"`
				} `json:"head"`
			} `json:"pull_request"`
			Repository struct {
				FullName string `json:"full_name"`
			} `json:"repository"`
		}
		if err := json.Unmarshal(body, &p); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid pull_request payload"})
			return
		}
		ev.Action = p.Action
		ev.Branch = p.PullRequest.Head.Ref
		ev.SHA = p.PullRequest.Head.SHA
		ev.RepoFullName = p.Repository.FullName
		// Previews build only same-repo PRs: a fork's head SHA is not fetchable
		// with the connection's credential, so the PR number is withheld and the
		// event degrades to the plain routing hook.
		if p.PullRequest.Head.Repo.FullName == "" ||
			strings.EqualFold(p.PullRequest.Head.Repo.FullName, p.Repository.FullName) {
			ev.PRNumber = p.Number
		}
	default:
		// Other subscribed events: still need the repo to route/audit.
		var p struct {
			Repository struct {
				FullName string `json:"full_name"`
			} `json:"repository"`
		}
		_ = json.Unmarshal(body, &p)
		ev.RepoFullName = p.Repository.FullName
	}

	outcome, err := s.git.HandleGitWebhook(r.Context(), ev)
	if err != nil {
		s.log.Error("webhook handling", "err", err, "delivery", deliveryID, "event", event)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// A preview teardown removed a resource — re-render its server so the
	// container and (pre-authorised) volume removal actually ship. Preview
	// deploys ride the ordinary deploy-request drain.
	if outcome.PreviewTeardown != nil && s.reconcile != nil {
		s.reconcile.ReconcileAsync(outcome.PreviewTeardown.OrgID, outcome.PreviewTeardown.ServerID)
	}

	resp := map[string]any{"delivered": true, "event": event}
	switch {
	case outcome.Duplicate:
		resp["status"] = "duplicate"
	case outcome.Connection == nil:
		resp["status"] = "repo not connected"
	case outcome.Enqueued != nil:
		resp["status"] = "deploy enqueued"
		resp["deployRequestId"] = outcome.Enqueued.ID
	case outcome.PreviewDeploy != nil:
		resp["status"] = "preview deploy enqueued"
		resp["deployRequestId"] = outcome.PreviewDeploy.ID
	case outcome.PreviewTeardown != nil:
		resp["status"] = "preview torn down"
	case outcome.PRHook != nil:
		resp["status"] = "pr recorded"
	default:
		resp["status"] = "acknowledged"
	}
	writeJSON(w, http.StatusOK, resp)
}

// validGitHubSignature checks the X-Hub-Signature-256 header (a "sha256=<hex>"
// HMAC of the raw body keyed by the shared secret) in constant time.
func validGitHubSignature(secret string, body []byte, header string) bool {
	const prefix = "sha256="
	if secret == "" || !strings.HasPrefix(header, prefix) {
		return false
	}
	want, err := hex.DecodeString(header[len(prefix):])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(want, mac.Sum(nil))
}
