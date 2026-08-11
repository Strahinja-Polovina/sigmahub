-- SIGMA-346: stamp the billing weight on the meter row.
--
-- server_hours recorded only (org_id, server_id, hour). The billed total's
-- historical arm therefore joined each metered hour back to the LIVE servers row
-- to find a weight:
--
--     (SELECT COALESCE(SUM(<weight(sv2.type)>), 0)
--        FROM (SELECT DISTINCT sh.server_id FROM server_hours sh
--               WHERE sh.org_id = … AND sh.hour >= …) d
--        JOIN servers sv2 ON sv2.id = d.server_id)
--
-- so the weight applied to an hour was whatever the server's type happened to be
-- when the query ran, not what it was when the hour was metered. Weights span
-- 1..4, and the type is user-mutable at any time through
-- POST /v1/orgs/{orgId}/servers/{serverId}/type.
--
-- Both directions were wrong. Downgrading a GPU server (weight 4) to a general
-- one (weight 1) inside the 24h window re-priced the whole window at 1, which
-- defeats for the WEIGHT exactly what the high-water mark exists to prevent for
-- the COUNT. Upgrading re-priced hours already metered at the higher weight, so
-- the quantity pushed to Paddle corresponded to nothing the customer had run.
--
-- The meter now carries its own weight and the arm sums that instead, so a
-- metered hour is immutable once written.
ALTER TABLE server_hours ADD COLUMN IF NOT EXISTS unit_weight INT NOT NULL DEFAULT 1;

-- Backfill from the type each server carries today. That is the best available
-- answer for hours metered before this column existed — their true weight was
-- never recorded — and it is exactly what the old query would have returned for
-- them, so the migration changes no figure on the day it runs. Only hours
-- metered from here on are immune to a later type change.
--
-- The CASE is written out rather than generated from the catalog on purpose: a
-- migration is a snapshot of a moment, runs once, and must keep meaning the same
-- thing when the catalog changes. unitWeightSQL() stays the single source of
-- truth for every query that runs more than once.
UPDATE server_hours sh
   SET unit_weight = CASE sv.type
                       WHEN 'gpu' THEN 4
                       WHEN 'k8s' THEN 2
                       ELSE 1
                     END
  FROM servers sv
 WHERE sv.id = sh.server_id
   AND sh.unit_weight = 1;
