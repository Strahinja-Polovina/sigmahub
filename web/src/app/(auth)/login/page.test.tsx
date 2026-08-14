// @vitest-environment jsdom
//
// The login screen carries the same SSO block as signup and needs the same rule
// (SIGMA-246): no provider configured, no buttons, no divider.

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

const push = vi.fn();
vi.mock("next/navigation", () => ({ useRouter: () => ({ push }) }));
const toastError = vi.fn();
vi.mock("sonner", () => ({
  toast: { info: vi.fn(), error: (...a: unknown[]) => toastError(...a), success: vi.fn() },
}));
vi.mock("@/lib/auth-client", () => ({
  authClient: {
    signIn: { email: vi.fn(), social: vi.fn() },
    sendVerificationEmail: vi.fn(),
  },
}));

import LoginPage from "./page";
import { authClient } from "@/lib/auth-client";
import { AuthProvidersProvider } from "@/components/auth/auth-providers";
import { MailDeliveryProvider } from "@/components/auth/mail-delivery";

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

// The dead end an upgrade creates (SIGMA-365).
//
// Verification now defaults ON wherever SMTP is configured, so a user whose
// sign-up mail was lost — or who was created in the window before the operator
// wired the transport — is refused here with EMAIL_NOT_VERIFIED. Rendered
// through the generic branch that reads "Invalid email or password", the only
// action the screen suggests is /forgot; a reset completes and sign-in is
// refused again for the same reason, because the block is not about the
// password. There is no other sender of a verification link in the product, so
// the user is out for good. This is where the way back in lives.
describe("LoginPage when the address is not verified", () => {
  const signIn = authClient.signIn.email as unknown as ReturnType<typeof vi.fn>;
  const resend = authClient.sendVerificationEmail as unknown as ReturnType<typeof vi.fn>;

  afterEach(() => vi.clearAllMocks());

  // By id, not by label: the password field carries a "Forgot password?" link
  // inside its label, so a /password/i label query is ambiguous here.
  const submit = async () => {
    const user = userEvent.setup();
    await user.type(document.getElementById("email")!, "ada@example.com");
    await user.type(document.getElementById("password")!, "correct horse battery");
    await user.click(screen.getByRole("button", { name: /^continue$/i }));
    return user;
  };

  it("names the real reason instead of blaming the password", async () => {
    signIn.mockResolvedValue({
      data: null,
      error: { status: 403, code: "EMAIL_NOT_VERIFIED", message: "Email not verified" },
    });

    render(
      <MailDeliveryProvider value={true}>
        <LoginPage />
      </MailDeliveryProvider>
    );
    await submit();

    await waitFor(() =>
      expect(screen.getByText(/confirm your email to continue/i)).toBeTruthy()
    );
    expect(screen.getByText(/ada@example.com/)).toBeTruthy();
    // A toast pointing at the password would send them to /forgot, which cannot
    // clear this.
    expect(toastError).not.toHaveBeenCalled();
    expect(push).not.toHaveBeenCalled();
  });

  it("can send a fresh link from here", async () => {
    signIn.mockResolvedValue({
      data: null,
      error: { status: 403, code: "EMAIL_NOT_VERIFIED", message: "Email not verified" },
    });
    resend.mockResolvedValue({ data: { status: true }, error: null });

    render(
      <MailDeliveryProvider value={true}>
        <LoginPage />
      </MailDeliveryProvider>
    );
    const user = await submit();

    await waitFor(() => expect(screen.getByText(/confirm your email/i)).toBeTruthy());
    await user.click(screen.getByRole("button", { name: /send it again/i }));

    await waitFor(() => expect(resend).toHaveBeenCalled());
    expect(resend.mock.calls[0][0]).toMatchObject({ email: "ada@example.com" });
  });

  it("keeps reporting a wrong password as a wrong password", async () => {
    signIn.mockResolvedValue({
      data: null,
      error: { status: 401, code: "INVALID_EMAIL_OR_PASSWORD", message: "Invalid" },
    });

    render(<LoginPage />);
    await submit();

    await waitFor(() => expect(toastError).toHaveBeenCalled());
    expect(screen.queryByText(/confirm your email/i)).toBeNull();
  });
});
