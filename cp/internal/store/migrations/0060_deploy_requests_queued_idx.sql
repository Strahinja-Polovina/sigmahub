-- SIGMA-317: the deploy drain sequentially scanned the whole push history every
-- three seconds.
--
-- runDeployDrain ticks every 3s — 28,800 times a day — and its driving query
-- filters `kind = 'deploy' AND status = 'queued'`. The only index on the table
-- was deploy_requests_org_idx (org_id, created_at), which cannot drive a
-- predicate on (kind, status), so every pass seq-scanned deploy_requests and
-- took row locks under FOR UPDATE ... SKIP LOCKED to discover there was nothing
-- to do. Requests move 'queued' → 'drained' within one tick and then live
-- forever, so the scanned set is the install's entire push history: ~200,000
-- rows on an active year-old install, ~67,000 rows/second of pure waste holding
-- one of the pool's connections continuously.
--
-- The cure is a PARTIAL index whose predicate is exactly the query's. It indexes
-- only rows that are actually waiting, so it stays near-empty no matter how much
-- history accumulates, and the drain's cost becomes proportional to the work
-- outstanding rather than to the install's age. created_at is the key so the
-- index also supplies the ORDER BY (oldest request first, which is the fairness
-- property the drain relies on).
CREATE INDEX IF NOT EXISTS deploy_requests_queued_idx
    ON deploy_requests (created_at)
    WHERE kind = 'deploy' AND status = 'queued';
