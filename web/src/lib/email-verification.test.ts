// Two callers, one verdict (SIGMA-365).
//
// better-auth's sign-in gate and the invite-accept gate in
// server/actions/members.ts both ask "does this deployment require a proven
// address?". They used to derive it separately — the flag on one side,
// mailDelivers() on the other — and every configuration where those two answers
// differ is a dead end for a real operator, in one direction or the other.

import { afterEach, describe, expect, it } from "vitest";

import { emailVerificationRequired, parseBoolEnv } from "./email-verification";

const vars = ["SMTP_HOST", "SMTP_FROM", "AUTH_REQUIRE_EMAIL_VERIFICATION"] as const;
afterEach(() => {
  for (const v of vars) delete process.env[v];
});

const withSmtp = () => {
  process.env.SMTP_HOST = "smtp.example.com";
  process.env.SMTP_FROM = "no-reply@example.com";
};

describe("emailVerificationRequired", () => {
  it("is off on a deployment that cannot deliver mail", () => {
    // The link would only reach the container log; requiring it would strand
    // every self-hosted sign-up.
    expect(emailVerificationRequired()).toBe(false);
  });

  it("is on once a transport is wired, without the operator asking", () => {
    withSmtp();
    expect(emailVerificationRequired()).toBe(true);
  });

  it("an explicit false wins over a wired transport", () => {
    // The configuration the invite gate used to break: SMTP is set, so
    // mailDelivers() is true, but no verification mail is ever sent and no
    // account is ever marked verified. A gate reading deliverability refused
    // EVERY invite acceptance here, forever, telling each invitee to look for a
    // link this deployment is configured never to send.
    withSmtp();
    process.env.AUTH_REQUIRE_EMAIL_VERIFICATION = "false";
    expect(emailVerificationRequired()).toBe(false);
  });

  it("an explicit true wins over a missing transport", () => {
    // The mirror image, and the one that matters for security: the operator has
    // asked for proof of address. A gate reading deliverability let an unproven
    // address accept an invite — the exact hole verification closes, since the
    // email match is all that binds an invite to a person.
    process.env.AUTH_REQUIRE_EMAIL_VERIFICATION = "true";
    expect(emailVerificationRequired()).toBe(true);
  });

  it("accepts every spelling Go's strconv.ParseBool does, and refuses the rest", () => {
    for (const v of ["1", "t", "T", "TRUE", "true", "True"]) {
      process.env.AUTH_REQUIRE_EMAIL_VERIFICATION = v;
      expect(emailVerificationRequired()).toBe(true);
    }
    for (const v of ["0", "f", "F", "FALSE", "false", "False"]) {
      process.env.AUTH_REQUIRE_EMAIL_VERIFICATION = v;
      expect(emailVerificationRequired()).toBe(false);
    }
    for (const v of ["yes", "on", "enabled", "TRUEE"]) {
      process.env.AUTH_REQUIRE_EMAIL_VERIFICATION = v;
      expect(() => emailVerificationRequired()).toThrow(
        /AUTH_REQUIRE_EMAIL_VERIFICATION/
      );
    }
  });
});

describe("parseBoolEnv", () => {
  it("returns the default only for unset and empty", () => {
    expect(parseBoolEnv("X", undefined, true)).toBe(true);
    expect(parseBoolEnv("X", "", true)).toBe(true);
    expect(parseBoolEnv("X", "  ", true)).toBe(true);
    expect(parseBoolEnv("X", "false", true)).toBe(false);
  });

  it("names the variable when it refuses a value", () => {
    // A security flag that quietly reads false after a typo is worse than one
    // that refuses to start — and the refusal has to say which one (SIGMA-142).
    expect(() => parseBoolEnv("CP_REQUIRE_ACTOR", "yes")).toThrow(/CP_REQUIRE_ACTOR/);
  });
});
