import { betterAuth } from "better-auth";
import { drizzleAdapter } from "better-auth/adapters/drizzle";
import { twoFactor } from "better-auth/plugins";
import { db, authSchema } from "../server/db";
import { sendMail } from "../server/email";
import { configuredAuthProviders } from "./auth-providers";
import { emailVerificationRequired } from "./email-verification";

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

// Invite email-match (acceptInvite) rests on the account's email actually
// belonging to the user; requiring a verified address is what closes that gap
// (SIGMA-82). The policy — the flag, its spellings, and its
// deliverability-derived default — lives in lib/email-verification.ts, because
// the invite gate in server/actions/members.ts has to reach exactly the same
// verdict and used to compute its own (SIGMA-365). Read once here: better-auth's
// options are fixed at construction, and a control that could change under a
// running process is not a control.
const requireEmailVerification = emailVerificationRequired();

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
    // A refused sign-in must hand back the way in, not just the reason it was
    // refused (SIGMA-365). better-auth's default is to answer EMAIL_NOT_VERIFIED
    // and send nothing, which leaves a user whose sign-up mail was lost — or who
    // signed up in the window before the operator wired SMTP — with no route to a
    // fresh link anywhere in the product: the only sender is sign-up, and
    // sign-up now answers a duplicate address with a deliberate no-op. With this
    // on, every attempt to sign in re-issues the link to the address that is
    // trying to use it, so the flow is self-healing. It reveals nothing: the
    // caller already proved the password on the line above.
    sendOnSignIn: true,
    // NOT auto-signed-in, though it is tempting and was briefly on (SIGMA-365).
    //
    // better-auth's /verify-email calls internalAdapter.createSession directly
    // when this is set — no second factor, ever. The twoFactor plugin works by
    // intercepting /sign-in/email and withholding the session until a TOTP code
    // arrives, so it is not on this path and cannot be. That makes a verification
    // link a complete sign-in for any account that is 2FA-enrolled but unverified,
    // which is a reachable combination: verification is off wherever no transport
    // is configured, so a user can sign up, enable 2FA, and only then have the
    // operator wire SMTP.
    //
    // Whoever holds the mailbox then holds the account — which is precisely the
    // compromise the second factor exists to survive. The cost of leaving it off
    // is one extra sign-in after verifying, and /signup says so rather than
    // promising a hop it no longer performs.
    autoSignInAfterVerification: false,
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
  // and reset/2FA probing are throttled.
  //
  // Storage is the DATABASE, not the default in-process map. The map bounds one
  // process, so with N replicas behind a proxy the effective sign-in limit
  // silently becomes N × 5/min — and it degrades with no signal anywhere, on a
  // change (scaling out) that nobody would connect to authentication. Making the
  // limit a property of the deployment rather than of a process means scaling is
  // a scaling decision, not a security one. Counters live in `rate_limit`
  // (server/db/auth-schema.ts) and better-auth prunes expired rows itself.
  //
  // This is the whole request path for the reference deployment. It does NOT
  // replace an edge rate limit or WAF for unauthenticated flooding — no
  // application-level limiter can, since the request has already reached the app
  // to be counted. That remains infrastructure, and `Caddyfile` carries the block.
  rateLimit: {
    enabled: true,
    storage: "database",
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
