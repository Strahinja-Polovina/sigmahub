package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
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

		if stored, err := s.domain.IdempotencyLookup(r.Context(), orgID, key); err == nil {
			replayStored(w, stored, reqHash)
			return
		} else if !errors.Is(err, store.ErrNotFound) {
			s.log.Error("idempotency lookup", "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		rec := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
		next(rec, r)

		// Persist only definitive outcomes; 5xx should be retryable.
		if rec.status < 500 {
			// WithoutCancel: a client disconnect/timeout mid-handler — the
			// exact case idempotency guards — must not skip persisting the
			// key, or the retry re-executes the mutation.
			stored, err := s.domain.IdempotencySave(context.WithoutCancel(r.Context()), orgID, key, store.IdempotentResponse{
				RequestHash: reqHash,
				StatusCode:  rec.status,
				Response:    rec.buf.Bytes(),
			})
			if err != nil {
				s.log.Error("idempotency save", "err", err)
				return
			}
			// A concurrent duplicate may have won the insert; both callers
			// still converge on one stored response (already written here).
			_ = stored
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
