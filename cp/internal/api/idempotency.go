package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"net/http"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// idempotent wraps an org-scoped POST handler with Idempotency-Key semantics:
// replaying the same (org, key) returns the stored response instead of
// re-executing; reusing a key with a different request body is a 409. The
// header is optional — requests without it pass straight through.
func (s *Server) idempotent(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if key == "" || len(key) > 200 {
			next(w, r)
			return
		}
		orgID := r.PathValue("orgId")

		// The body is needed twice (hash + handler), so buffer it.
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unreadable body"})
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		sum := sha256.Sum256(append([]byte(r.Method+" "+r.URL.Path+"\n"), body...))
		reqHash := sum[:]

		// Claim the key BEFORE executing (SIGMA-92): without an up-front claim,
		// two CONCURRENT requests with the same key both pass the lookup and both
		// run the mutation. Claiming atomically reserves the key so only the winner
		// executes; a concurrent duplicate replays (if finished) or is told to
		// retry (if still in flight).
		claimed, existing, err := s.domain.IdempotencyClaim(r.Context(), orgID, key, reqHash)
		if err != nil {
			s.log.Error("idempotency claim", "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		if !claimed {
			if !bytes.Equal(existing.RequestHash, reqHash) {
				writeJSON(w, http.StatusConflict, map[string]string{
					"error": "Idempotency-Key was already used with a different request",
				})
				return
			}
			if existing.Done {
				replayStored(w, existing, reqHash)
				return
			}
			// The same request is still in flight under this key — don't execute
			// it a second time; tell the client to retry for the stored response.
			w.Header().Set("Retry-After", "1")
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "a request with this Idempotency-Key is already in progress; retry shortly",
			})
			return
		}

		// This caller owns the claim. Release it if the handler doesn't finalize
		// (a 5xx or a panic) so a retry can re-execute instead of being wedged
		// "in progress".
		finalized := false
		defer func() {
			if !finalized {
				_ = s.domain.IdempotencyRelease(context.WithoutCancel(r.Context()), orgID, key)
			}
		}()

		rec := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
		next(rec, r)

		// Persist only definitive outcomes; 5xx should be retryable (→ released).
		if rec.status < 500 {
			// WithoutCancel: a client disconnect/timeout mid-handler — the exact
			// case idempotency guards — must not skip persisting the response, or
			// the retry re-executes the mutation.
			if err := s.domain.IdempotencyFinalize(context.WithoutCancel(r.Context()), orgID, key, rec.status, rec.buf.Bytes()); err != nil {
				s.log.Error("idempotency finalize", "err", err)
				return
			}
			finalized = true
		}
	}
}

func replayStored(w http.ResponseWriter, stored store.IdempotentResponse, reqHash []byte) {
	if !bytes.Equal(stored.RequestHash, reqHash) {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "Idempotency-Key was already used with a different request",
		})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Idempotency-Replayed", "true")
	w.WriteHeader(stored.StatusCode)
	_, _ = w.Write(stored.Response)
}

// responseRecorder tees the response so it can be persisted for replay.
type responseRecorder struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
}

func (r *responseRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	r.buf.Write(b)
	return r.ResponseWriter.Write(b)
}

// Unwrap/Flush keep the wrapped writer's streaming ability reachable through
// http.NewResponseController and a direct http.Flusher assertion (SIGMA-133).
func (r *responseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *responseRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
