-- SIGMA-81: bind daily S3 storage measurements to their resource.
--
-- s3_storage_bytes.resource_id carried no foreign key, so a deleted resource
-- left its per-day measurements behind — orphans that keep contributing to the
-- org-level billing aggregate (which integrates over (org_id, day)) forever.
-- Add an ON DELETE CASCADE FK so measurements die with their resource.
--
-- Drop any pre-existing orphans first, or ADD CONSTRAINT would fail on them.
DELETE FROM s3_storage_bytes b
 WHERE NOT EXISTS (SELECT 1 FROM resources r WHERE r.id = b.resource_id);

ALTER TABLE s3_storage_bytes
    ADD CONSTRAINT s3_storage_bytes_resource_fk
    FOREIGN KEY (resource_id) REFERENCES resources(id) ON DELETE CASCADE;

-- The sweep's per-bucket NOT EXISTS probe over
-- pending_s3_ops(resource_id, bucket, action='measure') is already served by
-- the partial index pending_s3_ops_measure_daily_uniq (migration 0030), whose
-- leading (resource_id, bucket) columns and action='measure' predicate match
-- the probe exactly — so no additional index is needed here.
