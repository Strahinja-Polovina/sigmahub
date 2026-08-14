import { betterAuth } from "better-auth";
import { drizzleAdapter } from "better-auth/adapters/drizzle";
import { twoFactor } from "better-auth/plugins";
import { db, authSchema } from "../server/db";
import { sendMail } from "../server/email";
import { configuredAuthProviders } from "./auth-providers";
import { mailDelivers } from "./mail";

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
// Default: ON wherever mail can actually be delivered (SIGMA-361/365). The flag
// was opt-in because no transport was bundled and turning it on would have
// stranded every sign-up at an unsendable verification link. Now that SMTP_HOST +
// SMTP_FROM wire a real transport, the deployment that can verify addresses
// should verify them by default — invite acceptance, audit rows and the whole
// "email is identity" assumption rest on it. A deployment with no transport keeps
// the old default (the link goes to the log and the operator relays it), and an
// explicit AUTH_REQUIRE_EMAIL_VERIFICATION still wins either way.
const requireEmailVerification = parseBoolEnv(
  "AUTH_REQUIRE_EMAIL_VERIFICATION",
  process.env.AUTH_REQUIRE_EMAIL_VERIFICATION,
  mailDelivers()
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
// Public origin of the dashboard. better-auth otherwise infers it from the
// incoming request headers, which behind the documented reverse proxy makes
// secure-cookie inference and origin validation depend on what the proxy
// forwards. Stating it makes both deterministic (SIGMA-365); unset keeps the
// previous inferred behaviour, which is what dev wants.
const baseURL = (process.env.BETTER_AUTH_URL ?? "").trim() || undefined;

export const auth = betterAuth({
  appName: "SigmaHub",
  secret,
  baseURL,
  trustedOrigins: baseURL ? [baseURL] : undefined,
  advanced: {
    // The dashboard is served over TLS in every hosted deployment; pin the
    // Secure attribute rather than letting it be inferred from a forwarded
    // header. NODE_ENV is the same signal the rest of the app uses.
    useSecureCookies: process.env.NODE_ENV === "production",
  },
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
    // Reset mail goes through the one door in server/email.ts, which submits
    // over SMTP when a transport is configured and logs the link otherwise —
    // the same answer lib/mail gives /forgot, so the copy ("check your inbox"
    // vs "ask your administrator for the link") can never describe a delivery
    // that did not happen (SIGMA-307/365).
    sendResetPassword: async ({ user, url }) => {
      await sendMail({
        to: user.email,
        subject: "Reset your SigmaHub password",
        text:
          `A password reset was requested for this address.\n\n` +
          `Reset it here:\n${url}\n\n` +
          `If this was not you, you can ignore this message — nothing changes ` +
          `until the link is used. Signing in again with your existing password ` +
          `also leaves it in place.\n`,
      });
    },
  },
  emailVerification: {
    // Only sent on sign-up when verification is actually required.
    sendOnSignUp: requireEmailVerification,
    sendVerificationEmail: async ({ user, url }) => {
      await sendMail({
        to: user.email,
        subject: "Verify your SigmaHub email address",
        text:
          `Confirm this address to finish setting up your SigmaHub account.\n\n` +
          `Verify:\n${url}\n\n` +
          `If you did not create an account, you can ignore this message.\n`,
      });
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
