// @vitest-environment jsdom
//
// SIGMA-345. authClient.changePassword had no call site anywhere in web/src and
// the Security tab rendered a single card — 2FA enrolment. There was no way to
// rotate a password while signed in, and (before SIGMA-344) no working way to
// reset one while locked out, so the password chosen at signup was the only one
// an account would ever have.

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

type ChangeArgs = {
  currentPassword: string;
  newPassword: string;
  revokeOtherSessions: boolean;
};

const changePassword = vi.fn(async (args: ChangeArgs) => ({
  data: { status: Boolean(args.newPassword) },
  error: null as { message: string } | null,
}));
vi.mock("@/lib/auth-client", () => ({
  authClient: {
    changePassword: (args: ChangeArgs) => changePassword(args),
    twoFactor: { enable: vi.fn(), disable: vi.fn(), verifyTotp: vi.fn() },
  },
}));
vi.mock("sonner", () => ({
  toast: Object.assign(vi.fn(), {
    error: vi.fn(),
    success: vi.fn(),
    message: vi.fn(),
  }),
}));

import { SecurityTab } from "./security-tab";

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

function fillAndSubmit(current: string, next: string, confirm = next) {
  fireEvent.change(screen.getByLabelText(/Current password/i), { target: { value: current } });
  fireEvent.change(screen.getByLabelText(/^New password$/i), { target: { value: next } });
  fireEvent.change(screen.getByLabelText(/Confirm new password/i), { target: { value: confirm } });
  fireEvent.click(screen.getByRole("button", { name: /Change password/i }));
}

describe("SecurityTab password card", () => {
  it("changes the password and revokes the other sessions", async () => {
    render(<SecurityTab initialTwoFactorEnabled={false} hasPassword />);

    fillAndSubmit("old-password", "correct horse battery");
    await waitFor(() => expect(changePassword).toHaveBeenCalled());

    // revokeOtherSessions is the whole point of rotating after a leak: without
    // it the session opened with the stolen password outlives the change.
    expect(changePassword).toHaveBeenCalledWith({
      currentPassword: "old-password",
      newPassword: "correct horse battery",
      revokeOtherSessions: true,
    });
  });

  it("does not submit when the confirmation does not match", async () => {
    render(<SecurityTab initialTwoFactorEnabled={false} hasPassword />);

    fillAndSubmit("old-password", "correct horse battery", "correct horse batteru");
    await waitFor(() => expect(changePassword).not.toHaveBeenCalled());
  });

  it("does not submit a new password shorter than the signup minimum", async () => {
    render(<SecurityTab initialTwoFactorEnabled={false} hasPassword />);

    fillAndSubmit("old-password", "short");
    await waitFor(() => expect(changePassword).not.toHaveBeenCalled());
  });

  it("hides the card for a social-only account with no credential to verify", () => {
    render(<SecurityTab initialTwoFactorEnabled={false} hasPassword={false} />);

    expect(screen.queryByRole("button", { name: /Change password/i })).toBeNull();
    // The 2FA card is still there — only the password form is withheld.
    expect(screen.getByText(/Two-factor authentication/i)).toBeTruthy();
  });
});
