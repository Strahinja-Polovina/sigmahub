// One answer to "does this deployment require a proven email address?"
// (SIGMA-365).
//
// It used to be computed in two places from two different facts. lib/auth.ts
// configured better-auth from AUTH_REQUIRE_EMAIL_VERIFICATION, defaulting to
// whether mail is deliverable; the invite-accept gate in
// server/actions/members.ts asked mailDelivers() directly. Those agree on the
// default and diverge the moment an operator states the flag — and both
// divergences are dead ends rather than tightenings or loosenings of a control:
//
//   SMTP wired + AUTH_REQUIRE_EMAIL_VERIFICATION=false — a real configuration,
//   for a deployment that wants reset mail without a verification step. No
//   verification mail is ever sent, so no account is ever marked verified, and
//   the invite gate — reading only "can mail be delivered" — refuses EVERY
//   acceptance, telling each invitee to check their inbox for a link that this
//   deployment has been configured never to send. Team onboarding is dead, and
//   nothing in the logs says why.
//
//   AUTH_REQUIRE_EMAIL_VERIFICATION=true with no transport — the operator has
//   asked for proof of address; the gate reads mailDelivers() as false and lets
//   an unproven address accept an invite. The email match is the only thing
//   binding an invite to a person, so that is the exact hole verification was
//   turned on to close.
//
// So both callers read this, and the question they ask is the one that matters:
// not "can we send mail" but "is a verified address required here".

import { mailDelivers } from "./mail";

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

/**
 * Whether sign-in and invite acceptance require a verified address.
 *
 * Default: ON wherever mail can actually be delivered (SIGMA-361/365). The flag
 * was opt-in because no transport was bundled and turning it on would have
 * stranded every sign-up at an unsendable verification link. Now that SMTP_HOST
 * + SMTP_FROM wire a real transport, the deployment that can verify addresses
 * should verify them by default — invite acceptance, audit rows and the whole
 * "email is identity" assumption rest on it. A deployment with no transport
 * keeps the old default (the link goes to the log and the operator relays it).
 *
 * It is parsed with parseBoolEnv, not `=== "true"`. The identical fail-open
 * construct on the control-plane side was SIGMA-142: an operator who wrote
 * CP_REQUIRE_ACTOR=1 got a silent `false` and ran with the security control off
 * while believing it was on.
 */
export function emailVerificationRequired(): boolean {
  return parseBoolEnv(
    "AUTH_REQUIRE_EMAIL_VERIFICATION",
    process.env.AUTH_REQUIRE_EMAIL_VERIFICATION,
    mailDelivers()
  );
}
