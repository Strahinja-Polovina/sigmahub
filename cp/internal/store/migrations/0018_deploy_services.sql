-- P1-9 compose multi-service: per-service deploy status.
--
-- A Compose app deploys N services from one deployment row. service_status maps
-- each service name to its state (deploying|success|failed); service_count is how
-- many services the deployment expects, so the row flips to success only once all
-- services succeed (and to failed the moment one does). Empty/0 for a
-- single-container app, whose status advances through the existing phase path.

ALTER TABLE deployments
    ADD COLUMN IF NOT EXISTS service_status jsonb NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN IF NOT EXISTS service_count  int   NOT NULL DEFAULT 0;
