-- SIGMA-298: a tombstone so an offboarded org stays offboarded.
--
-- Every other object in the control plane had a delete path; the org did not.
-- When a customer asked for erasure there was nothing to call, and the only
-- answers were "we cannot" or a manual wipe of a shared volume that also
-- destroyed every other tenant's data.
--
-- Deleting the rows is most of the job, but not all of it. The org id is chosen
-- by the dashboard, not by the control plane, so nothing stops the same id
-- being provisioned again after a teardown — and a re-provisioned org would
-- silently inherit its predecessor's telemetry tenant, because org_tenants
-- allocates a tenant per org id. That is how a deleted customer's log lines
-- come back under someone else's account.
--
-- Hence the tombstone: a row that outlives the deleted data, records who
-- deleted it and when, keeps the tenant number that was retired with it, and is
-- checked before an org id can be provisioned again. It holds no personal data
-- (an opaque id, a timestamp and an actor label), which is why keeping it is
-- compatible with the erasure it records.
CREATE TABLE IF NOT EXISTS org_tombstones (
    org_id      TEXT PRIMARY KEY,
    -- The telemetry tenant that was retired with the org. Nullable because an
    -- org that never sent a sample never had one allocated.
    tenant      INTEGER,
    deleted_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_by  TEXT        NOT NULL DEFAULT '',
    -- Free-text reason from the caller (e.g. "gdpr erasure request").
    reason      TEXT        NOT NULL DEFAULT ''
);
