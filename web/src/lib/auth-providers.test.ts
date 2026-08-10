// The env derivation behind the SSO buttons (SIGMA-246). Both halves of a
// credential are required: half a credential is a sign-in that fails at the
// redirect, which is worse than a button that never appeared.

import { describe, expect, it } from "vitest";

import { anyAuthProvider, configuredAuthProviders } from "./auth-providers";

describe("configuredAuthProviders", () => {
  it("configures nothing on a deployment that set nothing — today's default", () => {
    const flags = configuredAuthProviders({});
    expect(flags).toEqual({ google: false, github: false });
    expect(anyAuthProvider(flags)).toBe(false);
  });

  it("needs both halves of a credential", () => {
    expect(configuredAuthProviders({ AUTH_GITHUB_CLIENT_ID: "abc" }).github).toBe(false);
    expect(configuredAuthProviders({ AUTH_GITHUB_CLIENT_SECRET: "shh" }).github).toBe(false);
    expect(
      configuredAuthProviders({ AUTH_GITHUB_CLIENT_ID: "abc", AUTH_GITHUB_CLIENT_SECRET: "shh" })
        .github
    ).toBe(true);
  });

  it("treats blank and whitespace-only values as unset", () => {
    expect(
      configuredAuthProviders({ AUTH_GOOGLE_CLIENT_ID: "  ", AUTH_GOOGLE_CLIENT_SECRET: "shh" })
        .google
    ).toBe(false);
  });

  it("is per-provider, not all-or-nothing", () => {
    const flags = configuredAuthProviders({
      AUTH_GOOGLE_CLIENT_ID: "gid",
      AUTH_GOOGLE_CLIENT_SECRET: "gsec",
    });
    expect(flags).toEqual({ google: true, github: false });
    expect(anyAuthProvider(flags)).toBe(true);
  });
});
