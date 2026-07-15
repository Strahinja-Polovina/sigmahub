package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// IdempotentResponse is a stored POST outcome, replayed when the same
// (org, Idempotency-Key) arrives again.
type IdempotentResponse struct {
	RequestHash []byte
	StatusCode  int
	Response    []byte
}

// IdempotencyLookup returns the stored response for (org, key), or ErrNotFound.
func (s *Store) IdempotencyLookup(ctx context.Context, orgID, key string) (IdempotentResponse, error) {
	var r IdempotentResponse
	err := s.Pool.QueryRow(ctx, `
		SELECT request_hash, status_code, response
		  FROM idempotency_keys WHERE org_id = $1 AND key = $2`, orgID, key,
	).Scan(&r.RequestHash, &r.StatusCode, &r.Response)
	if errors.Is(err, pgx.ErrNoRows) {
		return IdempotentResponse{}, ErrNotFound
	}
	return r, err
}

// IdempotencySave stores a POST outcome. On a concurrent duplicate the first
// writer wins and the stored row is returned so both callers converge on one
// response.
func (s *Store) IdempotencySave(ctx context.Context, orgID, key string, in IdempotentResponse) (IdempotentResponse, error) {
	tag, err := s.Pool.Exec(ctx, `
		INSERT INTO idempotency_keys (org_id, key, request_hash, status_code, response)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (org_id, key) DO NOTHING`,
		orgID, key, in.RequestHash, in.StatusCode, in.Response)
	if err != nil {
		return IdempotentResponse{}, err
	}
	if tag.RowsAffected() == 0 {
		return s.IdempotencyLookup(ctx, orgID, key)
	}
	return in, nil
}
