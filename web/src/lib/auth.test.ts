// AUTH_REQUIRE_EMAIL_VERIFICATION is a security control, so it is parsed the
// way the control plane parses CP_REQUIRE_ACTOR (SIGMA-142): every spelling Go's
// strconv.ParseBool accepts turns it on, and anything unrecognised fails boot.
// The old `=== "true"` check left "1"/"True"/"TRUE" silently false — the
// operator believed verification was on while sign-in kept accepting unverified
// addresses, which is exactly the fail-open the CP-side table in
// cp/internal/config/config_test.go (TestRequireActorFromEnv) exists to prevent.

import { afterEach, describe, expect, it, vi } from "vitest";

type AuthOptions = {
  emailAndPassword?: { requireEmailVerification?: boolean };
  emailVerification?: { sendOnSignUp?: boolean };
};

/** Re-import lib/auth with the flag set to `value` (undefined = unset) and hand
 *  back what better-auth was actually configured with — asserting on the option
 *  better-auth holds, not on our own intermediate variable. */
async function loadAuthOptions(value: string | undefined): Promise<AuthOptions> {
  vi.resetModules();
  vi.stubEnv("AUTH_REQUIRE_EMAIL_VERIFICATION", value);
  const mod = await import("./auth");
  return (mod.auth as unknown as { options: AuthOptions }).options;
}

afterEach(() => {
  vi.unstubAllEnvs();
  vi.resetModules();
});

describe("AUTH_REQUIRE_EMAIL_VERIFICATION accepts 1/True and rejects typos", () => {
  for (const [value, want] of [
    [undefined, false],
    ["", false],
    ["true", true],
    ["1", true],
    ["True", true],
    ["TRUE", true],
    ["t", true],
    ["false", false],
    ["0", false],
    ["False", false],
  ] as const) {
    it(`parses ${JSON.stringify(value)} as ${want}`, async () => {
      const opts = await loadAuthOptions(value);
      expect(opts.emailAndPassword?.requireEmailVerification).toBe(want);
      // The verification mail on sign-up follows the same flag: a deployment
      // that requires verification must actually send the link.
      expect(opts.emailVerification?.sendOnSignUp).toBe(want);
    });
  }

  for (const value of ["yes", "enabled", "on", "TRUEE"]) {
    it(`fails module load on ${JSON.stringify(value)} instead of silently disabling the control`, async () => {
      vi.resetModules();
      vi.stubEnv("AUTH_REQUIRE_EMAIL_VERIFICATION", value);
      await expect(import("./auth")).rejects.toThrow(/AUTH_REQUIRE_EMAIL_VERIFICATION/);
    });
  }
});
