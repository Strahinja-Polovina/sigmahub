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
    twoFactor: { verifyTotp: vi.fn(), verifyBackupCode: vi.fn() },
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

// The permanent lockout (SIGMA-365).
//
// Settings → Security generates ten backup codes, shows them once, and tells the
// user to save them — the whole point being a lost authenticator. The TOTP step
// was the ONLY second-factor screen in the product and it submitted exactly one
// endpoint, verifyTotp. `verifyBackupCode` existed in the installed client and
// was called from nowhere, and better-auth mints codes as `xxxxx-xxxxx` —
// eleven characters with a hyphen — which the six-digit segmented input strips
// down to nothing typeable.
//
// So a user holding a valid, unspent credential the product minted and told them
// to keep had no field anywhere to type it into. Every exit needs a session,
// getting a session needs the TOTP, a password reset does not clear 2FA, and a
// sole Org Admin locked out freezes the whole tenant — members.ts refuses to
// leave an org with zero admins. Recovery was an operator UPDATE by hand.
describe("LoginPage second factor", () => {
  const signIn = authClient.signIn.email as unknown as ReturnType<typeof vi.fn>;
  const verifyTotp = authClient.twoFactor.verifyTotp as unknown as ReturnType<typeof vi.fn>;
  const verifyBackupCode = authClient.twoFactor
    .verifyBackupCode as unknown as ReturnType<typeof vi.fn>;

  afterEach(() => vi.clearAllMocks());

  /** Sign in as a 2FA-enrolled user, landing on the TOTP step. */
  const toTotpStep = async () => {
    signIn.mockResolvedValue({ data: { twoFactorRedirect: true }, error: null });
    render(<LoginPage />);
    const user = userEvent.setup();
    await user.type(document.getElementById("email")!, "ada@example.com");
    await user.type(document.getElementById("password")!, "correct horse battery");
    await user.click(screen.getByRole("button", { name: /^continue$/i }));
    await waitFor(() => expect(screen.getByText(/two-factor authentication/i)).toBeTruthy());
    return user;
  };

  it("redeems a backup code — the credential the product told the user to save", async () => {
    verifyBackupCode.mockResolvedValue({ data: {}, error: null });
    const user = await toTotpStep();

    await user.click(screen.getByRole("button", { name: /use a backup code/i }));
    await user.type(document.getElementById("backup-code")!, "k3f9a-2mq7z");
    await user.click(screen.getByRole("button", { name: /verify and continue/i }));

    await waitFor(() => expect(verifyBackupCode).toHaveBeenCalled());
    expect(verifyBackupCode.mock.calls[0][0]).toMatchObject({ code: "k3f9a-2mq7z" });
    // It must NOT go through the TOTP endpoint, which would reject it.
    expect(verifyTotp).not.toHaveBeenCalled();
    await waitFor(() => expect(push).toHaveBeenCalled());
  });

  it("does not apply the 6-digit rule to a backup code", async () => {
    // validateOtp would refuse an 11-character alphanumeric value out of hand,
    // which is what made the code unenterable in the first place.
    verifyBackupCode.mockResolvedValue({ data: {}, error: null });
    const user = await toTotpStep();
    await user.click(screen.getByRole("button", { name: /use a backup code/i }));
    await user.type(document.getElementById("backup-code")!, "k3f9a-2mq7z");
    await user.click(screen.getByRole("button", { name: /verify and continue/i }));

    await waitFor(() => expect(verifyBackupCode).toHaveBeenCalled());
    expect(screen.queryByText(/6-digit|6 digits/i)).toBeNull();
  });

  it("offers no fake Resend on the TOTP step", async () => {
    // What used to sit there made no network call and toasted "a new code is on
    // its way to your device". There is nothing to resend for TOTP, and it was
    // aimed precisely at the user who is locked out.
    await toTotpStep();
    expect(screen.queryByRole("button", { name: /resend/i })).toBeNull();
  });

  it("still verifies an ordinary TOTP code", async () => {
    verifyTotp.mockResolvedValue({ data: {}, error: null });
    const user = await toTotpStep();
    // Submitting the authenticator path with nothing entered must still be held
    // to the 6-digit rule — the backup-code path must not have loosened it.
    await user.click(screen.getByRole("button", { name: /verify and continue/i }));
    // The heading also says "6-digit"; the error is the one in destructive text.
    await waitFor(() => expect(screen.getByText(/^Enter the 6-digit code\.$/)).toBeTruthy());
    expect(verifyBackupCode).not.toHaveBeenCalled();
    expect(verifyTotp).not.toHaveBeenCalled();
  });
});
