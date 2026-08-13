import { betterAuth } from "better-auth";
import { drizzleAdapter } from "better-auth/adapters/drizzle";
import { twoFactor } from "better-auth/plugins";
import { db, authSchema } from "../server/db";
import { configuredAuthProviders } from "./auth-providers";

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

// Boolean env parsing that mirrors cp/internal/config.parseBoolEnv, which in
// turn mirrors Go's strconv.ParseBool: the set of accepted spellings is the
// same on both sides of the product, so an operator who writes `=1` in the CP
// section of their .env and `=1` in the web section gets the same answer twice.
// Unset/empty is the documented default; anything else that is not a recognised
// spelling throws, because a security flag that quietly reads `false` after a
// typo is worse than one that refuses to start.
const TRUTHY = new Set(["1", "t", "T", "TRUE", "true", "True"]);
const FALSY = new Set(["0", "f", "F", "FALSE", "false", "False"]);

export function parseBoolEnv(key: string, raw: string | undefined, def = false): boolean {
  const v = (raw ?? "").trim();
  if (v === "") return def;
  if (TRUTHY.has(v)) return true;
  if (FALSY.has(v)) return false;
  throw new Error(`${key} must be a boolean (true/false), got ${JSON.stringify(raw)}`);
}

// Invite email-match (acceptInvite) rests on the account's email actually
// belonging to the user. AUTH_REQUIRE_EMAIL_VERIFICATION=true makes better-auth
// block sign-in until the address is verified, closing that gap (SIGMA-82). It
// is opt-in: no SMTP is bundled (verification links go to the server log, like
// the reset flow), so turning it on without a real transport would strand
// sign-ups — a deliberate operator choice once a transport is wired, not a
// silent default that breaks the beta.
//
// It is parsed with parseBoolEnv, not `=== "true"`. The identical fail-open
// construct on the control-plane side was SIGMA-142: an operator who wrote
// CP_REQUIRE_ACTOR=1 got a silent `false` and ran with the security control off
// while believing it was on. This flag is the same shape of control on the same
// SIGMA-82 gap, so it gets the same contract — every spelling Go's
// strconv.ParseBool accepts turns it on, and an unrecognised value fails boot
// loudly instead of leaving sign-in open to unverified addresses with no signal
// anywhere in the logs or the UI (SIGMA-261).
const requireEmailVerification = parseBoolEnv(
  "AUTH_REQUIRE_EMAIL_VERIFICATION",
  process.env.AUTH_REQUIRE_EMAIL_VERIFICATION
);

// Third-party sign-in, from the same flags the auth screens read (SIGMA-246).
// Empty on a deployment that has set no OAuth credentials — which is every
// deployment today — and that is exactly why the Google/GitHub buttons no
// longer render there. Configuring the credentials wires better-auth and lights
// up the buttons in one step, so a visible button always has a flow behind it.
const providerFlags = configuredAuthProviders();
const socialProviders = {
  ...(providerFlags.google
    ? {
        google: {
          clientId: process.env.AUTH_GOOGLE_CLIENT_ID!,
          clientSecret: process.env.AUTH_GOOGLE_CLIENT_SECRET!,
        },
      }
    : {}),
  ...(providerFlags.github
    ? {
        github: {
          clientId: process.env.AUTH_GITHUB_CLIENT_ID!,
          clientSecret: process.env.AUTH_GITHUB_CLIENT_SECRET!,
        },
      }
    : {}),
};

// v1 dev: better-auth over the same PGlite DB (email+password + TOTP 2FA).
// Orgs/memberships live in our own tables (V1-1); better-auth owns user auth.
// Production points DATABASE_URL at a real Postgres behind the same adapter.
export const auth = betterAuth({
  appName: "SigmaHub",
  secret,
  database: drizzleAdapter(db, { provider: "pg", schema: authSchema }),
  emailAndPassword: {
    enabled: true,
    requireEmailVerification,
    // A reset is what someone does when the old password is in the wrong hands,
    // so the sessions opened with it must not survive it (SIGMA-344). Off by
    // default in better-auth, which would have left an attacker's session live
    // through the victim's recovery — and /reset-password tells the user their
    // other sessions ended, so this is also what makes that sentence true.
    revokeSessionsOnPasswordReset: true,
    // Reset emails: no SMTP is bundled, so the reset link goes to the server
    // log — genuinely usable in dev/self-hosted setups (the operator can
    // relay it), and honest about what happens instead of silently dropping.
    // Wire a real transport here for hosted deployments — and in the SAME
    // commit teach lib/mail.ts about it, because that is what /forgot reads to
    // decide whether to say "check your inbox" or "ask your administrator for
    // the link" (SIGMA-307). A transport wired here and not there puts the
    // locked-out user back in their spam folder.
    sendResetPassword: async ({ user, url }) => {
      console.info(`[auth] password reset requested for ${user.email}: ${url}`);
    },
  },
  emailVerification: {
    // Same honest log transport as the reset flow (wire real SMTP for hosted).
    // Only sent on sign-up when verification is actually required.
    sendOnSignUp: requireEmailVerification,
    sendVerificationEmail: async ({ user, url }) => {
      console.info(`[auth] email verification for ${user.email}: ${url}`);
    },
  },
  socialProviders,
  // Rate limiting on the auth surface (SIGMA-365). better-auth enables a limiter
  // only in production by default; turn it on explicitly (dev too) and hold the
  // credential-guessing endpoints well below the global rate so password spraying
  // and reset/2FA probing are throttled. Storage is in-memory, which bounds a
  // SINGLE instance — a horizontally-scaled deployment should point this at shared
  // (database/secondary) storage so the limit holds across replicas, and the edge
  // should still carry its own rate limit / WAF (both tracked in SIGMA-365).
  rateLimit: {
    enabled: true,
    window: 60,
    max: 100,
    customRules: {
      "/sign-in/email": { window: 60, max: 5 },
      "/sign-up/email": { window: 60, max: 5 },
      "/forget-password": { window: 60, max: 3 },
      "/two-factor/verify": { window: 60, max: 5 },
    },
  },
  plugins: [twoFactor()],
});

export type Session = typeof auth.$Infer.Session;
