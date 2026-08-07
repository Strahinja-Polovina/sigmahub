ALTER TABLE "environments" ADD COLUMN "production" boolean DEFAULT false NOT NULL;--> statement-breakpoint
ALTER TABLE "memberships" ADD COLUMN "scoped" boolean DEFAULT false NOT NULL;--> statement-breakpoint
ALTER TABLE "resources" ADD COLUMN "ephemeral" boolean DEFAULT false NOT NULL;--> statement-breakpoint
ALTER TABLE "servers" ADD COLUMN "mesh_ip" text DEFAULT '' NOT NULL;--> statement-breakpoint
-- SIGMA-167 backfill: members already holding a project grant were scoped
-- under the old count-based rule; carry that state into the explicit flag so
-- their visibility is unchanged by this migration.
UPDATE "memberships" m SET "scoped" = true WHERE EXISTS (
  SELECT 1 FROM "project_memberships" pm
  JOIN "projects" p ON pm."project_id" = p."id"
  WHERE pm."user_id" = m."user_id" AND p."org_id" = m."org_id"
);
