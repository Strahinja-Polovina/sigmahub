// @vitest-environment jsdom
//
// SIGMA-344. /forgot used to send the reset link with redirectTo: "/login".
// better-auth's GET /reset-password/<token> verifies the token and then
// redirects to that callbackURL carrying ?token=… — it sets no password itself.
// So the locked-out user landed on the sign-in form with a live reset token in
// the query string that nothing read, and POST /reset-password had no caller
// anywhere in the app. Recovery was impossible in-product.
//
// These cover the page that now sits at the end of that link.

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

let search = new URLSearchParams();
vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn() }),
  useSearchParams: () => search,
}));

type ResetArgs = { newPassword: string; token: string };
type ResetResult = { data: { status: boolean } | null; error: { message: string } | null };

const resetPassword = vi.fn(
  async (args: ResetArgs): Promise<ResetResult> => ({
    data: { status: Boolean(args.token) },
    error: null,
  }),
);
vi.mock("@/lib/auth-client", () => ({
  authClient: { resetPassword: (args: ResetArgs) => resetPassword(args) },
}));

import ResetPasswordPage from "./page";

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  search = new URLSearchParams();
});

function fill(newPassword: string, confirm = newPassword) {
  fireEvent.change(screen.getByLabelText(/^New password$/i), {
    target: { value: newPassword },
  });
  fireEvent.change(screen.getByLabelText(/Confirm new password/i), {
    target: { value: confirm },
  });
  fireEvent.click(screen.getByRole("button", { name: /Set new password/i }));
}

describe("ResetPasswordPage", () => {
  it("sets the new password using the token from the link", async () => {
    search = new URLSearchParams("token=tok_live_123");
    render(<ResetPasswordPage />);

    fill("correct horse battery");
    await waitFor(() => expect(resetPassword).toHaveBeenCalled());

    expect(resetPassword).toHaveBeenCalledWith({
      newPassword: "correct horse battery",
      token: "tok_live_123",
    });
    // And the user is told they can now use it, rather than being left guessing.
    expect(await screen.findByText(/Password updated/i)).toBeTruthy();
  });

  it("refuses to submit when the confirmation does not match", async () => {
    search = new URLSearchParams("token=tok_live_123");
    render(<ResetPasswordPage />);

    fill("correct horse battery", "correct horse batteru");
    await waitFor(() => expect(screen.getByText(/do not match/i)).toBeTruthy());
    expect(resetPassword).not.toHaveBeenCalled();
  });

  it("enforces the same minimum length as signup", async () => {
    search = new URLSearchParams("token=tok_live_123");
    render(<ResetPasswordPage />);

    fill("short");
    await waitFor(() => expect(screen.getByText(/at least 8 characters/i)).toBeTruthy());
    expect(resetPassword).not.toHaveBeenCalled();
  });

  it("explains an expired link and offers a fresh one instead of a dead form", async () => {
    // What better-auth redirects with when the token is spent or past its hour.
    search = new URLSearchParams("error=INVALID_TOKEN");
    render(<ResetPasswordPage />);

    expect(screen.getByText(/isn’t valid|isn't valid/i)).toBeTruthy();
    // Rendered as an anchor carrying role="button" by the Button primitive.
    const again = screen.getByRole("button", { name: /Request a new link/i });
    expect(again.getAttribute("href")).toBe("/forgot");
    expect(screen.queryByRole("button", { name: /Set new password/i })).toBeNull();
  });

  it("does the same when the URL carries no token at all", async () => {
    render(<ResetPasswordPage />);

    expect(screen.getByText(/isn’t valid|isn't valid/i)).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Set new password/i })).toBeNull();
  });

  it("surfaces a rejected token rather than reporting success", async () => {
    search = new URLSearchParams("token=tok_dead");
    resetPassword.mockResolvedValueOnce({ data: null, error: { message: "invalid token" } });
    render(<ResetPasswordPage />);

    fill("correct horse battery");
    await waitFor(() => expect(screen.getByRole("alert")).toBeTruthy());
    expect(screen.queryByText(/Password updated/i)).toBeNull();
  });
});
