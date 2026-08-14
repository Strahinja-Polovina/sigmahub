-- SIGMA-365: a real bound on outbound mail per organization.
--
-- The first attempt at this bounded the wrong things and did not hold. A
-- per-invitation resend cooldown bounds how often ONE row is mailed; a cap on
-- invitations CREATED per hour bounds how many new rows appear. Neither bounds
-- total volume, and the product of the two is the actual limit: 25 pending
-- invitations, each resendable once a minute, is 1500 messages an hour from one
-- organization — which is a mail cannon with extra steps.
--
-- Counting sends is the only thing that bounds sends. One row per org, an atomic
-- upsert-and-increment on every message, and a window that rolls forward on its
-- own. No cleanup job: the row is bounded by the number of orgs and is reset in
-- place rather than accumulated, and it cascades away with the org.
--
-- window_start is when the current window opened, NOT when the last message was
-- sent — a sliding "last send" would let a steady drip reset the window forever.
CREATE TABLE IF NOT EXISTS "org_mail_budget" (
	"org_id" text PRIMARY KEY NOT NULL,
	"window_start" timestamp DEFAULT now() NOT NULL,
	"sent" integer DEFAULT 0 NOT NULL
);
--> statement-breakpoint
DO $$ BEGIN
 ALTER TABLE "org_mail_budget" ADD CONSTRAINT "org_mail_budget_org_id_orgs_id_fk" FOREIGN KEY ("org_id") REFERENCES "public"."orgs"("id") ON DELETE cascade ON UPDATE no action;
EXCEPTION
 WHEN duplicate_object THEN null;
END $$;
