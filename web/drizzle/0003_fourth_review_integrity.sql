-- SIGMA-111: collapse any pre-existing duplicate memberships before the unique
-- constraint (keep one row per (org_id,user_id)). Duplicates should not exist,
-- but the constraint would fail to apply if the acceptInvite race ever produced
-- one, wedging the migration.
DELETE FROM "memberships" a
 USING "memberships" b
 WHERE a."org_id" = b."org_id"
   AND a."user_id" = b."user_id"
   AND a.ctid > b.ctid;
--> statement-breakpoint
ALTER TABLE "memberships" ADD CONSTRAINT "memberships_org_id_user_id_unique" UNIQUE("org_id","user_id");--> statement-breakpoint
-- SIGMA-115: collapse duplicate PENDING invites for the same (org, email) before
-- the partial unique index, keeping the most recent link.
DELETE FROM "invitations" a
 USING "invitations" b
 WHERE a."status" = 'pending' AND b."status" = 'pending'
   AND a."org_id" = b."org_id"
   AND lower(a."email") = lower(b."email")
   AND (a."created_at" < b."created_at"
        OR (a."created_at" = b."created_at" AND a.ctid > b.ctid));
--> statement-breakpoint
CREATE UNIQUE INDEX "invitations_org_pending_email_uniq" ON "invitations" USING btree ("org_id",lower("email")) WHERE "invitations"."status" = 'pending';
