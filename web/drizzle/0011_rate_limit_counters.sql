-- SIGMA-365: give better-auth's rate limiter somewhere shared to count.
--
-- The limiter was on but stored its counters in an in-process map. That bounds
-- ONE process. It is the whole request path for the single-container reference
-- deployment, and it stops being true the moment `web` is scaled: with N
-- replicas behind a proxy the effective sign-in limit becomes N × 5/min. It
-- degrades silently — nothing logs that the limit stopped holding, and the
-- operator who scaled out had no reason to connect a replica count to
-- authentication. Password spraying is exactly the attack that notices.
--
-- Backing it with the database makes the limit a property of the deployment
-- rather than of a process. The table is better-auth's own `rateLimit` model:
-- it writes `key`, `count` and `last_request` through the drizzle adapter, and
-- prunes expired rows itself as it goes.
--
-- `last_request` is epoch MILLISECONDS, not a timestamp — the limiter compares
-- it against Date.now() arithmetic, and the adapter reads bigint back as a
-- number. Storing it as timestamptz would typecheck and then silently never
-- expire a window.
--
-- The UNIQUE on "key" is load-bearing, not tidiness. better-auth's consume path
-- inserts and CATCHES the conflict to re-read the existing row; with only an
-- ordinary index two concurrent requests both insert, the read takes whichever
-- row comes back first, and the counters split — so the limit stops tripping
-- exactly under the concurrency an attacker supplies, while every serial test
-- still passes.
CREATE TABLE IF NOT EXISTS "rate_limit" (
	"id" text PRIMARY KEY NOT NULL,
	"key" text NOT NULL,
	"count" integer DEFAULT 0 NOT NULL,
	"last_request" bigint NOT NULL,
	CONSTRAINT "rate_limit_key_unique" UNIQUE("key")
);
--> statement-breakpoint
CREATE INDEX IF NOT EXISTS "rateLimit_key_idx" ON "rate_limit" USING btree ("key");
