-- SIGMA-174: git_connections_repo_idx was UNIQUE on (provider, lower(repo_full_name))
-- with NO org scope, even though the row carries org_id and every other
-- tenant-scoped uniqueness in this schema is org- or project-scoped. On a
-- multi-tenant install that let any project admin in any org "connect" a repo
-- they cannot access and permanently occupy the global slot: the real owner got
-- an unexplained 409 forever, and — because delivery routing resolved by repo
-- name alone — once the owner installed the GitHub App, their push/PR metadata
-- routed into the squatter's org.
--
-- Scope uniqueness to the org. No de-dup pass is needed: the old global index
-- was strictly stronger, so existing data cannot violate the new one. Delivery
-- routing is org-disambiguated in code (gitConnectionForDeliveryTx) via the
-- github_installations binding (SIGMA-87), falling back to the unique global
-- match; an ambiguous delivery is dropped rather than guessed.
DROP INDEX IF EXISTS git_connections_repo_idx;
CREATE UNIQUE INDEX IF NOT EXISTS git_connections_org_repo_idx
    ON git_connections (org_id, provider, lower(repo_full_name));
