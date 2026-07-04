import { betterAuth } from "better-auth";
import { drizzleAdapter } from "better-auth/adapters/drizzle";
import { twoFactor } from "better-auth/plugins";
import { db, authSchema } from "../server/db";

// v1 dev: better-auth over the same PGlite DB (email+password + TOTP 2FA).
// Orgs/memberships live in our own tables (V1-1); better-auth owns user auth.
// Prod: swap the adapter to GCP Identity Platform behind the same call sites.
export const auth = betterAuth({
  appName: "SigmaHub",
  secret: process.env.BETTER_AUTH_SECRET,
  database: drizzleAdapter(db, { provider: "pg", schema: authSchema }),
  emailAndPassword: { enabled: true },
  plugins: [twoFactor()],
});

export type Session = typeof auth.$Infer.Session;
