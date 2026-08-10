// @vitest-environment jsdom
//
// Revoking a service token invalidates an org credential immediately: every
// CI job, script and integration authenticating with it starts failing on the
// next request, and the token cannot be un-revoked. It used to fire off a
// single click on a small red button sitting beside Rotate (SIGMA-311).
import * as React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

vi.mock("sonner", () => {
  const toast = Object.assign(vi.fn(), {
    success: vi.fn(),
    error: vi.fn(),
    info: vi.fn(),
    warning: vi.fn(),
  });
  return { toast };
});
vi.mock("@/server/actions/service-tokens", () => ({
  listServiceTokens: vi.fn(),
  rotateServiceToken: vi.fn(),
  revokeServiceToken: vi.fn(),
}));

import { listServiceTokens, revokeServiceToken } from "@/server/actions/service-tokens";
import { TokensTab } from "./tokens-tab";
import type { CpServiceToken } from "@/server/cp";

const CI: CpServiceToken = {
  id: "tok_1",
  name: "ci-deploy",
  role: "Deployer",
  createdBy: "ada",
  createdAt: "2026-08-01T10:00:00Z",
  lastUsedAt: null,
  revokedAt: null,
};

function renderTab(tokens: CpServiceToken[] = [CI]) {
  vi.mocked(listServiceTokens).mockResolvedValue(tokens);
  return render(<TokensTab orgId="org_1" isAdmin />);
}

/** The row's Revoke button, once the initial load transition has settled — it
 *  renders disabled while `pending` is true, and a click on a disabled button
 *  is silently dropped. */
async function revokeButton(): Promise<HTMLButtonElement> {
  const btn = (await screen.findByRole("button", { name: /^Revoke$/ })) as HTMLButtonElement;
  await waitFor(() => expect(btn.disabled).toBe(false));
  return btn;
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("TokensTab revoke", () => {
  it("revoking a token requires confirmation and says integrations break immediately", async () => {
    renderTab();
    fireEvent.click(await revokeButton());

    expect(revokeServiceToken).not.toHaveBeenCalled();

    const dialog = await screen.findByRole("dialog");
    expect(dialog.textContent).toContain("ci-deploy");
    expect(dialog.textContent).toMatch(/stops working immediately/i);

    fireEvent.click(screen.getByRole("button", { name: /^Revoke token$/ }));
    expect(revokeServiceToken).toHaveBeenCalledWith({
      orgId: "org_1",
      tokenId: "tok_1",
      name: "ci-deploy",
    });
  });

  it("cancelling the confirmation revokes nothing", async () => {
    renderTab();
    fireEvent.click(await revokeButton());
    fireEvent.click(screen.getByRole("button", { name: /^Cancel$/ }));
    expect(revokeServiceToken).not.toHaveBeenCalled();
  });
});
