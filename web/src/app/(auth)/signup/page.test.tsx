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
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

const push = vi.fn();
vi.mock("next/navigation", () => ({ useRouter: () => ({ push }) }));
vi.mock("sonner", () => ({ toast: { info: vi.fn(), error: vi.fn(), success: vi.fn() } }));
vi.mock("@/lib/auth-client", () => ({
  authClient: {
    signUp: { email: vi.fn() },
    signIn: { social: vi.fn() },
    sendVerificationEmail: vi.fn(),
  },
}));

import SignupPage from "./page";
import { authClient } from "@/lib/auth-client";
import { AuthProvidersProvider } from "@/components/auth/auth-providers";
import { MailDeliveryProvider } from "@/components/auth/mail-delivery";

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

// The other half of the screen's honesty (SIGMA-365).
//
// With AUTH_REQUIRE_EMAIL_VERIFICATION on — now the default wherever SMTP is
// configured — better-auth's /sign-up/email returns 200 with a NULL token and
// sets no session cookie. This page pushed to the dashboard on any non-error,
// so the user got "Account created — welcome to SigmaHub", a redirect, and an
// immediate bounce back to /login from the (auth) layout's session check. They
// were left holding working credentials, no session, and no mention anywhere
// that a link was waiting in their inbox.
describe("SignupPage when the address has to be verified first", () => {
  const fill = async () => {
    const user = userEvent.setup();
    await user.type(screen.getByLabelText(/full name/i), "Ada Lovelace");
    await user.type(screen.getByLabelText(/work email/i), "ada@example.com");
    await user.type(screen.getByLabelText(/^password$/i), "correct horse battery");
    await user.type(screen.getByLabelText(/confirm password/i), "correct horse battery");
    await user.click(screen.getByRole("button", { name: /create account/i }));
    return user;
  };

  const signUp = authClient.signUp.email as unknown as ReturnType<typeof vi.fn>;
  const resend = authClient.sendVerificationEmail as unknown as ReturnType<typeof vi.fn>;

  afterEach(() => {
    vi.clearAllMocks();
  });

  it("shows the check-your-inbox screen instead of a dashboard it has no session for", async () => {
    signUp.mockResolvedValue({ data: { token: null, user: {} }, error: null });

    render(
      <MailDeliveryProvider value={true}>
        <SignupPage />
      </MailDeliveryProvider>
    );
    await fill();

    await waitFor(() => expect(screen.getByText(/verify your email/i)).toBeTruthy());
    expect(screen.getByText(/ada@example.com/)).toBeTruthy();
    // The redirect is the bug: there is no session to redirect into.
    expect(push).not.toHaveBeenCalled();
  });

  it("offers a resend, which is the only route to a fresh link the product has", async () => {
    signUp.mockResolvedValue({ data: { token: null, user: {} }, error: null });
    resend.mockResolvedValue({ data: { status: true }, error: null });

    render(
      <MailDeliveryProvider value={true}>
        <SignupPage />
      </MailDeliveryProvider>
    );
    const user = await fill();

    await waitFor(() => expect(screen.getByText(/verify your email/i)).toBeTruthy());
    await user.click(screen.getByRole("button", { name: /resend the link/i }));

    await waitFor(() => expect(resend).toHaveBeenCalled());
    expect(resend.mock.calls[0][0]).toMatchObject({ email: "ada@example.com" });
  });

  it("says where the link really went on a deployment that sends no mail", async () => {
    // Verification can be forced on with no transport wired. Telling that user
    // to check an inbox is the SIGMA-307 failure again, on a new screen.
    signUp.mockResolvedValue({ data: { token: null, user: {} }, error: null });

    render(
      <MailDeliveryProvider value={false}>
        <SignupPage />
      </MailDeliveryProvider>
    );
    await fill();

    await waitFor(() =>
      expect(screen.getByText(/email delivery isn.t configured/i)).toBeTruthy()
    );
    expect(screen.getByText(/written to the dashboard server.s log/i)).toBeTruthy();
    expect(screen.queryByText(/spam folder/i)).toBeNull();
  });

  it("still goes straight in when sign-up did return a session", async () => {
    // Verification off — the flow this page was written for — must not regress.
    signUp.mockResolvedValue({ data: { token: "sess_abc", user: {} }, error: null });

    render(<SignupPage />);
    await fill();

    await waitFor(() => expect(push).toHaveBeenCalledWith("/dashboard"));
    expect(screen.queryByText(/verify your email/i)).toBeNull();
  });
});
