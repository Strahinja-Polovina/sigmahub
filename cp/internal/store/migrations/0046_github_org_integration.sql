-- Make the GitHub App installation a first-class ORG-LEVEL integration.
--
-- Until now an installation was only an opaque id bound to an org (SIGMA-87) and
-- referenced per git_connection, so every repo had to be connected by hand —
-- there was nothing to render as "GitHub is connected to this organization" and
-- no way to list the repos an org can already deploy. These columns carry the
-- account the App is installed on plus who connected it and when, so the
-- dashboard can show the integration and offer a repo picker instead of a
-- connect-a-repo form.
ALTER TABLE github_installations
    ADD COLUMN IF NOT EXISTS account_login TEXT   NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS account_type  TEXT   NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS created_by    TEXT   NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS updated_at    TIMESTAMPTZ NOT NULL DEFAULT now();

-- Listing an org's integrations is the dashboard's hot path.
CREATE INDEX IF NOT EXISTS github_installations_org_idx
    ON github_installations (org_id, created_at DESC);

-- A connection created from the repo picker is derived, not hand-made: record
-- it so the UI can explain why a connection exists and so cleaning up an
-- integration knows which connections it implied.
ALTER TABLE git_connections
    ADD COLUMN IF NOT EXISTS auto_connected BOOLEAN NOT NULL DEFAULT false;
