// @vitest-environment jsdom
//
// SIGMA-307. No SMTP transport is bundled: sendResetPassword writes the reset
// URL to the web container's stdout and returns. The page nevertheless said
// "Check your inbox … we've sent a link to reset your password" and then
// "Didn't get the email? Check your spam folder or try again."
//
// A self-hosted deployment's second user therefore spends a day searching a
// spam folder for mail that was never sent, while the link sits in a log they
// have no reason to read and probably no access to. The invite flow in the same
// product already refuses to do this — sendInviteEmail returns
// { delivered: false } and the dialog says delivery isn't configured — so this
// screen gets the same honesty.

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

vi.mock("next/navigation", () => ({ useRouter: () => ({ push: vi.fn() }) }));
const requestPasswordReset = vi.fn(async () => ({ data: null, error: null }));
vi.mock("@/lib/auth-client", () => ({
  authClient: { requestPasswordReset: () => requestPasswordReset() },
}));

import ForgotPasswordPage from "./page";
import { MailDeliveryProvider } from "@/components/auth/mail-delivery";

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

async function submitEmail() {
  fireEvent.change(screen.getByLabelText(/Email/i), {
    target: { value: "ada@acme.example" },
  });
  fireEvent.click(screen.getByRole("button", { name: /Send reset link/i }));
  await waitFor(() => expect(requestPasswordReset).toHaveBeenCalled());
}

describe("ForgotPasswordPage", () => {
  it("with no mail transport configured the page does not claim an email was sent", async () => {
    // No provider = the safe default, which is also every deployment today.
    render(<ForgotPasswordPage />);
    await submitEmail();

    const body = document.body.textContent ?? "";
    expect(body).not.toMatch(/check your inbox/i);
    expect(body).not.toMatch(/we've sent|we’ve sent/i);
    expect(body).not.toMatch(/spam folder/i);

    // What it says instead: why nothing arrived, and who can get them in.
    expect(screen.getByText(/delivery isn’t configured|delivery isn't configured/i)).toBeTruthy();
    expect(body).toMatch(/administrator/i);
  });

  it("with a transport configured it still tells the user to check their inbox", async () => {
    render(
      <MailDeliveryProvider value={true}>
        <ForgotPasswordPage />
      </MailDeliveryProvider>
    );
    await submitEmail();

    expect(screen.getByText(/check your inbox/i)).toBeTruthy();
    expect(document.body.textContent ?? "").toMatch(/spam folder/i);
  });
});
