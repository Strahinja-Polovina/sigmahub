-- SIGMA-365: remember when an invite was last mailed, so it can be throttled.
--
-- `resendInvite` had no limit of any kind. An org admin — a role anyone gets by
-- signing up, since sign-up creates a personal org with the user as Org Admin —
-- could hold the resend button and send unbounded mail to any address they had
-- invited, from OUR sending domain. That is not a tenant's problem: the
-- reputation being spent is the reputation every other tenant's password-reset
-- mail depends on, and the recipient does not have to be a customer. `inviteMember`
-- is bounded to one pending invite per address, but not in how many distinct
-- addresses an org may mail per hour.
--
-- On a public launch this is the cheapest abuse in the product to run and the
-- most expensive to undo, because a blocklisted sending domain is not fixed by
-- deploying a patch.
--
-- Throttling needs the last send time on the row, which is a fact about the
-- invitation that was simply never recorded — created_at is not it, because a
-- resend does not create a row. Backfilled from created_at rather than now() so
-- an invite created an hour ago is not treated as if it were just mailed.
ALTER TABLE "invitations" ADD COLUMN IF NOT EXISTS "last_sent_at" timestamp;
--> statement-breakpoint
UPDATE "invitations" SET "last_sent_at" = "created_at" WHERE "last_sent_at" IS NULL;
--> statement-breakpoint
ALTER TABLE "invitations" ALTER COLUMN "last_sent_at" SET NOT NULL;
--> statement-breakpoint
ALTER TABLE "invitations" ALTER COLUMN "last_sent_at" SET DEFAULT now();
--> statement-breakpoint
-- The hourly per-org cap counts rows by creation time; this is the index that
-- keeps that count off a sequential scan as the table grows.
CREATE INDEX IF NOT EXISTS "invitations_org_created_idx" ON "invitations" USING btree ("org_id","created_at");
