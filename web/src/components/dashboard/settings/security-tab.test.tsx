// @vitest-environment jsdom
//
// SIGMA-345. authClient.changePassword had no call site anywhere in web/src and
// the Security tab rendered a single card — 2FA enrolment. There was no way to
// rotate a password while signed in, and (before SIGMA-344) no working way to
// reset one while locked out, so the password chosen at signup was the only one
// an account would ever have.

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

type EnableResult = {
  data: { totpURI: string; backupCodes: string[] } | null;
  error: { message: string } | null;
};

const enable = vi.fn(async (a: { password: string }): Promise<EnableResult> => ({
  data: a.password
    ? { totpURI: "otpauth://totp/SigmaHub:ada?secret=JBSWY3DPEHPK3PXP", backupCodes: ["aaaa-1111", "bbbb-2222"] }
    : null,
  error: null,
}));
const verifyTotp = vi.fn(async (a: { code: string }) => ({
  data: { status: a.code.length === 6 },
  error: null,
}));
const generateBackupCodes = vi.fn(async (a: { password: string }) => ({
  data: { status: Boolean(a.password), backupCodes: ["cccc-3333", "dddd-4444"] },
  error: null as { message: string } | null,
}));

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
    twoFactor: {
      enable: (a: { password: string }) => enable(a),
      disable: vi.fn(),
      verifyTotp: (a: { code: string }) => verifyTotp(a),
      generateBackupCodes: (a: { password: string }) => generateBackupCodes(a),
    },
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

// SIGMA-347: the codes panel lived inside the `totpUri &&` block, and confirm()
// cleared totpUri — so the ten codes the user was told to save vanished on the
// same click that finished enrolment, with no way to mint more.
describe("SecurityTab backup codes", () => {
  async function enrol() {
    fireEvent.change(screen.getByLabelText(/Account password/i), {
      target: { value: "old-password" },
    });
    fireEvent.click(screen.getByRole("button", { name: /Enable 2FA/i }));
    await waitFor(() => expect(screen.getByText("aaaa-1111")).toBeTruthy());
  }

  it("keeps the codes on screen after enrolment is confirmed", async () => {
    render(<SecurityTab initialTwoFactorEnabled={false} hasPassword={false} />);
    await enrol();

    fireEvent.change(screen.getByLabelText(/6-digit code/i), { target: { value: "123456" } });
    fireEvent.click(screen.getByRole("button", { name: /Confirm/i }));
    await waitFor(() => expect(verifyTotp).toHaveBeenCalled());

    // The secret panel is gone; the codes are not.
    expect(screen.getByText("aaaa-1111")).toBeTruthy();
    expect(screen.getByText("bbbb-2222")).toBeTruthy();
  });

  it("only hides them once the user says they saved them", async () => {
    render(<SecurityTab initialTwoFactorEnabled={false} hasPassword={false} />);
    await enrol();

    fireEvent.click(screen.getByRole("button", { name: /saved these/i }));
    await waitFor(() => expect(screen.queryByText("aaaa-1111")).toBeNull());
  });

  it("can mint a fresh set later, so losing them is recoverable", async () => {
    render(<SecurityTab initialTwoFactorEnabled hasPassword={false} />);

    fireEvent.change(screen.getByLabelText(/Account password/i), {
      target: { value: "old-password" },
    });
    fireEvent.click(screen.getByRole("button", { name: /New backup codes/i }));
    await waitFor(() => expect(generateBackupCodes).toHaveBeenCalledWith({ password: "old-password" }));

    expect(await screen.findByText("cccc-3333")).toBeTruthy();
  });
});
