package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// IdempotentResponse is a stored POST outcome, replayed when the same
// (org, Idempotency-Key) arrives again. Done is false for a CLAIMED but not yet
// finalized row (a request currently in flight under this key).
type IdempotentResponse struct {
	RequestHash []byte
	StatusCode  int
	Response    []byte
	Done        bool
}

// IdempotencyLookup returns the stored row for (org, key), or ErrNotFound. A
// claimed-but-unfinished row is returned with Done=false and a zero
// StatusCode/Response.
func (s *Store) IdempotencyLookup(ctx context.Context, orgID, key string) (IdempotentResponse, error) {
	var r IdempotentResponse
	var status *int
	err := s.Pool.QueryRow(ctx, `
		SELECT request_hash, status_code, response
		  FROM idempotency_keys WHERE org_id = $1 AND key = $2`, orgID, key,
	).Scan(&r.RequestHash, &status, &r.Response)
	if errors.Is(err, pgx.ErrNoRows) {
		return IdempotentResponse{}, ErrNotFound
	}
	if err != nil {
		return IdempotentResponse{}, err
	}
	if status != nil {
		r.StatusCode = *status
		r.Done = true
	}
	return r, nil
}

// IdempotencyClaim atomically reserves (org, key) BEFORE the handler runs, so two
// CONCURRENT requests with the same key can't both execute the mutation
// (SIGMA-92). claimed=true means this caller won the reservation and must run the
// handler then Finalize (or Release on failure). claimed=false returns the
// existing row: replay it when existing.Done, else the same request is still in
// flight under this key.
func (s *Store) IdempotencyClaim(ctx context.Context, orgID, key string, reqHash []byte) (bool, IdempotentResponse, error) {
	tag, err := s.Pool.Exec(ctx, `
		INSERT INTO idempotency_keys (org_id, key, request_hash)
		VALUES ($1, $2, $3) ON CONFLICT (org_id, key) DO NOTHING`,
		orgID, key, reqHash)
	if err != nil {
		return false, IdempotentResponse{}, err
	}
	if tag.RowsAffected() == 1 {
		return true, IdempotentResponse{}, nil
	}
	existing, err := s.IdempotencyLookup(ctx, orgID, key)
	if err != nil {
		return false, IdempotentResponse{}, err
	}
	return false, existing, nil
}

// IdempotencyFinalize writes the completed response onto a claimed row so
// subsequent replays return it.
func (s *Store) IdempotencyFinalize(ctx context.Context, orgID, key string, statusCode int, response []byte) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE idempotency_keys SET status_code = $3, response = $4
		 WHERE org_id = $1 AND key = $2`, orgID, key, statusCode, response)
	return err
}

// IdempotencyRelease drops a claimed-but-unfinished row (the handler 5xx'd or
// panicked) so a later retry can re-execute instead of being stuck "in
// progress". Only deletes a pending row (response IS NULL) — never a finalized one.
func (s *Store) IdempotencyRelease(ctx context.Context, orgID, key string) error {
	_, err := s.Pool.Exec(ctx, `
		DELETE FROM idempotency_keys WHERE org_id = $1 AND key = $2 AND response IS NULL`,
		orgID, key)
	return err
}
