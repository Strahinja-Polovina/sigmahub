-- SIGMA-204, demo side: the mirror learns that a disconnect takes time.
--
-- Disconnecting used to be one instant: the row went away. It is now a
-- conversation with the machine — the control plane asks, the agent tears the
-- host down and answers, and only then is the row tombstoned — so the mirror
-- needs somewhere to hold the in-flight request.
--
-- decommission_started_at is the clock. Both the "Force disconnect" affordance
-- and the demo's timeout simulation are comparisons against it, which a bare
-- status value cannot answer.
--
-- decommission_purge_volumes is whether the operator opted into destroying
-- named volumes. It is stored rather than only passed through so a page reload
-- mid-teardown still tells the truth about what is being deleted.
ALTER TABLE "servers" ADD COLUMN "decommission_started_at" timestamp;--> statement-breakpoint
ALTER TABLE "servers" ADD COLUMN "decommission_purge_volumes" boolean DEFAULT false NOT NULL;
