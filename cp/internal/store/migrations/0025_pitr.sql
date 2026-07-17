-- P2-5 phase A: PostgreSQL PITR groundwork. pitr_enabled turns on continuous
-- WAL archiving (container archives segments into a spool volume; the agent
-- ships them into the resource's restic repo) plus a daily physical base
-- backup run — the two ingredients a point-in-time restore replays from.
-- wal_archive_status is the honest health signal the dashboard shows: the
-- PITR window only reaches as far as the last shipped segment.
ALTER TABLE backup_policies ADD COLUMN pitr_enabled BOOLEAN NOT NULL DEFAULT FALSE;

CREATE TABLE wal_archive_status (
    resource_id TEXT PRIMARY KEY REFERENCES resources(id) ON DELETE CASCADE,
    org_id TEXT NOT NULL,
    last_segment TEXT NOT NULL DEFAULT '',
    last_shipped_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
