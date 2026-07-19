-- SIGMA-87: bind a GitHub App installation to the org that first claims it, so a
-- client-supplied installationId can't drive the CP to mint an installation
-- token for an installation another org owns. First-writer-wins; cross-org reuse
-- is rejected at detect/connect/link time.
CREATE TABLE IF NOT EXISTS github_installations (
    installation_id TEXT PRIMARY KEY,
    org_id          TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Pre-bind installations already referenced by existing connections to their
-- org, so an upgrade doesn't leave them unclaimed (and raceable). If the same
-- installation somehow appears under multiple orgs, keep one deterministically.
INSERT INTO github_installations (installation_id, org_id)
SELECT installation_id, min(org_id)
  FROM git_connections
 WHERE installation_id IS NOT NULL AND installation_id <> ''
 GROUP BY installation_id
ON CONFLICT (installation_id) DO NOTHING;
