-- SIGMA-92: allow an Idempotency-Key row to be CLAIMED before its response
-- exists, so two concurrent requests with the same key can't both execute the
-- mutation. A pending claim has NULL status_code/response; it is finalized with
-- the response when the handler completes, or deleted if the handler 5xx'd.
ALTER TABLE idempotency_keys ALTER COLUMN status_code DROP NOT NULL;
ALTER TABLE idempotency_keys ALTER COLUMN response DROP NOT NULL;
