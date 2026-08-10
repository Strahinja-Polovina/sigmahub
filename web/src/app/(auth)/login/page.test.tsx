// @vitest-environment jsdom
//
// The login screen carries the same SSO block as signup and needs the same rule
// (SIGMA-246): no provider configured, no buttons, no divider.

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

vi.mock("next/navigation", () => ({ useRouter: () => ({ push: vi.fn() }) }));
vi.mock("sonner", () => ({ toast: { info: vi.fn(), error: vi.fn(), success: vi.fn() } }));
vi.mock("@/lib/auth-client", () => ({
  authClient: { signIn: { email: vi.fn(), social: vi.fn() } },
}));

import LoginPage from "./page";
import { AuthProvidersProvider } from "@/components/auth/auth-providers";

afterEach(cleanup);

describe("LoginPage", () => {
  it("offers no SSO when no provider is configured", () => {
    render(<LoginPage />);

    expect(screen.queryByRole("button", { name: /continue with google/i })).toBeNull();
    expect(screen.queryByRole("button", { name: /continue with github/i })).toBeNull();
    expect(screen.queryByRole("button", { name: /with a passkey/i })).toBeNull();
    expect(screen.queryByText(/or continue with/i)).toBeNull();
    // The credentials form is untouched.
    expect(screen.getByRole("button", { name: /^continue$/i })).toBeTruthy();
  });

  it("offers a configured provider", () => {
    render(
      <AuthProvidersProvider value={{ google: true, github: false }}>
        <LoginPage />
      </AuthProvidersProvider>
    );

    expect(screen.getByRole("button", { name: /continue with google/i })).toBeTruthy();
    expect(screen.getByText(/or continue with/i)).toBeTruthy();
  });
});
