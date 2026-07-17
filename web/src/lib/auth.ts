import { betterAuth } from "better-auth";
import { drizzleAdapter } from "better-auth/adapters/drizzle";
import { twoFactor } from "better-auth/plugins";
import { db, authSchema } from "../server/db";

// Session/token signing key. better-auth falls back to an insecure default
// when unset — acceptable only in dev; a production SERVER must fail fast, or
// every session cookie is forgeable. `next build` also runs with
// NODE_ENV=production but has no runtime secrets — skip the check there
// (NEXT_PHASE marks the build).
const secret = process.env.BETTER_AUTH_SECRET;
if (
  process.env.NODE_ENV === "production" &&
  process.env.NEXT_PHASE !== "phase-production-build" &&
  !secret
) {
  throw new Error(
    "BETTER_AUTH_SECRET is required in production (generate one with `openssl rand -base64 32`)."
  );
}

// v1 dev: better-auth over the same PGlite DB (email+password + TOTP 2FA).
// Orgs/memberships live in our own tables (V1-1); better-auth owns user auth.
// Production points DATABASE_URL at a real Postgres behind the same adapter.
export const auth = betterAuth({
  appName: "SigmaHub",
  secret,
  database: drizzleAdapter(db, { provider: "pg", schema: authSchema }),
  emailAndPassword: {
    enabled: true,
    // Reset emails: no SMTP is bundled, so the reset link goes to the server
    // log — genuinely usable in dev/self-hosted setups (the operator can
    // relay it), and honest about what happens instead of silently dropping.
    // Wire a real transport here for hosted deployments.
    sendResetPassword: async ({ user, url }) => {
      console.info(`[auth] password reset requested for ${user.email}: ${url}`);
    },
  },
  plugins: [twoFactor()],
});

export type Session = typeof auth.$Infer.Session;
