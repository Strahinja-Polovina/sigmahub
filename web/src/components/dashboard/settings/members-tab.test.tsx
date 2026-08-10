// @vitest-environment jsdom
//
// Removing a member is irreversible in the part that matters: `removeMember`
// deletes the membership AND every project grant the user held in this org
// (server/actions/members.ts), and re-inviting them restores the membership but
// not the grants — nothing anywhere records what they were. The control that
// fires it lives one divider below the role list in a compact dropdown, so a
// mis-click while changing someone's role used to destroy all of it with no
// second step (SIGMA-311).
import * as React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";

vi.mock("sonner", () => {
  const toast = Object.assign(vi.fn(), {
    success: vi.fn(),
    error: vi.fn(),
    info: vi.fn(),
    warning: vi.fn(),
  });
  return { toast };
});
vi.mock("@/server/actions/members", () => ({
  changeMemberRole: vi.fn(),
  inviteMember: vi.fn(),
  removeMember: vi.fn(),
  resendInvite: vi.fn(),
  revokeInvite: vi.fn(),
}));
vi.mock("@/server/actions/project-members", () => ({
  restoreOrgWideAccess: vi.fn(),
}));

import { removeMember } from "@/server/actions/members";
import { MembersTab } from "./members-tab";
import type { SettingsMember } from "./settings-view";

const ADA: SettingsMember = {
  id: "usr_ada",
  name: "Ada Lovelace",
  email: "ada@example.com",
  role: "Developer",
  scoped: true,
  grantCount: 3,
};

function renderTab(members: SettingsMember[] = [ADA]) {
  return render(
    <MembersTab
      orgId="org_1"
      members={members}
      pendingInvites={[]}
      currentUserId="usr_admin"
      isAdmin
    />
  );
}

/** Open the row's action menu and click "Remove from org". */
async function clickRemove(name = ADA.name) {
  fireEvent.click(screen.getByRole("button", { name: `Manage ${name}` }));
  fireEvent.click(await screen.findByText("Remove from org"));
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("MembersTab remove", () => {
  it("removing a member requires confirmation and names the grant count", async () => {
    renderTab();
    await clickRemove();

    // The menu item must not be the action. It opens a dialog.
    expect(removeMember).not.toHaveBeenCalled();

    const dialog = await screen.findByRole("dialog");
    // What dies has to be on screen: the person, and the grants that cannot be
    // reconstructed afterwards.
    expect(dialog.textContent).toContain("Ada Lovelace");
    expect(dialog.textContent).toMatch(/3 project grants/);

    fireEvent.click(screen.getByRole("button", { name: /^Remove member$/ }));
    expect(removeMember).toHaveBeenCalledWith({ orgId: "org_1", userId: "usr_ada" });
  });

  it("cancelling the confirmation removes nobody", async () => {
    renderTab();
    await clickRemove();
    fireEvent.click(screen.getByRole("button", { name: /^Cancel$/ }));
    expect(removeMember).not.toHaveBeenCalled();
  });

  it("a member with no project grants is described without an invented count", async () => {
    renderTab([{ ...ADA, scoped: false, grantCount: 0 }]);
    await clickRemove();
    const dialog = await screen.findByRole("dialog");
    expect(dialog.textContent).not.toMatch(/0 project grants/);
    expect(dialog.textContent).toMatch(/no project grants/i);
  });

  it("a single grant is counted in the singular", async () => {
    renderTab([{ ...ADA, grantCount: 1 }]);
    await clickRemove();
    const dialog = await screen.findByRole("dialog");
    expect(dialog.textContent).toMatch(/1 project grant\b/);
  });
});
