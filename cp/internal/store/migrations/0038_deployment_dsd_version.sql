-- SIGMA-134: record the DSD version that first rendered each deployment. The
-- op-status path advances "the in-flight deployment" for a (server,resource),
-- but op ids carry no deployment identity, so a late status report from a
-- SUPERSEDED deployment (an older DSD version) was applied to the newer in-flight
-- one, fabricating its terminal status. Stamping the render version lets the
-- advance reject a report whose version predates the in-flight deployment.
-- 0 = not yet rendered / legacy row (the gate treats 0 as "unknown" and skips).
ALTER TABLE deployments ADD COLUMN IF NOT EXISTS dsd_version BIGINT NOT NULL DEFAULT 0;
