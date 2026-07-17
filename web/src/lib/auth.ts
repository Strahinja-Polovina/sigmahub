import { betterAuth } from "better-auth";
import { drizzleAdapter } from "better-auth/adapters/drizzle";
import { twoFactor } from "better-auth/plugins";
import { db, authSchema } from "../server/db";

// v1 dev: better-auth over the same PGlite DB (email+password + TOTP 2FA).
// Orgs/memberships live in our own tables (V1-1); better-auth owns user auth.
// Prod: swap the adapter to GCP Identity Platform behind the same call sites.
export const auth = betterAuth({
  appName: "SigmaHub",
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
