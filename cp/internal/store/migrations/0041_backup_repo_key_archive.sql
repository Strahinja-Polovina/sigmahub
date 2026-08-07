-- SIGMA-170: the per-resource restic repo password lived ONLY on the
-- backup_policies row, and backup_policies.resource_id cascades from resources
-- (which cascade from projects/environments). Deleting a database — or the
-- project it sat in — therefore destroyed the only key to that customer's
-- offsite snapshots, while the snapshots themselves stayed in their bucket,
-- still billing, now permanently undecryptable. The product already treats
-- "destroy this database's live bytes" as confirm-gated and leaves the Docker
-- volumes alone; it was destroying the key to the offsite copies with an
-- unguarded DELETE.
--
-- This table keeps the wrapped key alive across the cascade. It is org-scoped
-- and deliberately has NO foreign key to resources or backup_policies: the whole
-- point is to outlive them. org_id is a bare TEXT like every other org-scoped CP
-- table (backup_policies, org_deks, usage_hours) — organizations live in the web
-- app's own database, so there is nothing here to reference. The ciphertext keeps
-- its original policy id so the existing repoKeyAAD(org, policy) binding — and
-- the KEK-rotation re-wrap — work on archived rows unchanged.
CREATE TABLE IF NOT EXISTS backup_repo_key_archive (
	policy_id           TEXT PRIMARY KEY,
	org_id              TEXT NOT NULL,
	resource_id         TEXT NOT NULL,
	resource_name       TEXT NOT NULL DEFAULT '',
	repo_key_ciphertext BYTEA NOT NULL,
	repo_key_nonce      BYTEA NOT NULL,
	repo_dek_id         TEXT REFERENCES org_deks (id),
	archived_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS backup_repo_key_archive_resource_idx
	ON backup_repo_key_archive (org_id, resource_id);
