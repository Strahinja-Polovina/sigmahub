// @vitest-environment jsdom
//
// What the very first screen a design partner sees is allowed to offer
// (SIGMA-246).
//
// Signup rendered "Sign up with Google", "Sign up with GitHub" and "Sign up with
// a passkey" under an "or sign up with" divider, unconditionally. None of them
// worked: lib/auth.ts configures emailAndPassword and twoFactor only, so every
// handler was a toast reading "Single sign-on is not wired up in this
// prototype." A developer evaluating a tool whose whole pitch is deploying from
// GitHub clicks the GitHub button first — and is told the product is a
// prototype before they have an account.
//
// So the rule this file pins: the block appears only for providers a deployment
// has actually configured, and the divider goes with it.

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

vi.mock("next/navigation", () => ({ useRouter: () => ({ push: vi.fn() }) }));
vi.mock("sonner", () => ({ toast: { info: vi.fn(), error: vi.fn(), success: vi.fn() } }));
vi.mock("@/lib/auth-client", () => ({
  authClient: { signUp: { email: vi.fn() }, signIn: { social: vi.fn() } },
}));

import SignupPage from "./page";
import { AuthProvidersProvider } from "@/components/auth/auth-providers";

const SSO_BUTTONS = [/sign up with google/i, /sign up with github/i, /with a passkey/i];

// No AuthProvidersProvider around the page = the context default, which is
// "nothing configured" — the same thing the (auth) layout passes on a
// deployment that has set no OAuth credentials, i.e. every one today.
afterEach(cleanup);

describe("SignupPage", () => {
  it("no SSO buttons render when no provider is configured", () => {
    render(<SignupPage />);

    for (const name of SSO_BUTTONS) {
      expect(screen.queryByRole("button", { name })).toBeNull();
    }
    // The divider is part of the offer — it must not be left hanging over
    // nothing.
    expect(screen.queryByText(/or sign up with/i)).toBeNull();

    // The path that does work is untouched.
    expect(screen.getByRole("button", { name: /create account/i })).toBeTruthy();
  });

  it("a configured provider lights up — and only that one", () => {
    render(
      <AuthProvidersProvider value={{ google: false, github: true }}>
        <SignupPage />
      </AuthProvidersProvider>
    );

    expect(screen.getByRole("button", { name: /sign up with github/i })).toBeTruthy();
    expect(screen.queryByRole("button", { name: /sign up with google/i })).toBeNull();
    expect(screen.getByText(/or sign up with/i)).toBeTruthy();
  });
});
