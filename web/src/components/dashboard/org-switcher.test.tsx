// @vitest-environment jsdom
//
// SIGMA-306. The org switcher's last item is "New organization" with a Plus
// icon — and it was a <Link href="/dashboard/settings">, i.e. a link to the
// CURRENT org's settings. The Settings → General tab it lands on shows the
// current org's name in an editable field with a Save button, so the obvious
// next move ("type the new client's name, press Save") RENAMES the org the user
// already had, for every teammate at once. There was no in-product path to a
// second org at all and nothing anywhere said so.
//
// These tests ask the only two questions that matter: does the menu item start
// a create flow (rather than navigate into a rename form), and does that flow
// call the action that actually creates one.

import * as React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

const refresh = vi.fn();
const push = vi.fn();
vi.mock("next/navigation", () => ({
  useRouter: () => ({ refresh, push, replace: vi.fn() }),
}));
vi.mock("sonner", () => {
  const toast = Object.assign(vi.fn(), {
    success: vi.fn(),
    error: vi.fn(),
    info: vi.fn(),
    warning: vi.fn(),
  });
  return { toast };
});
// Actions answer with a result rather than throwing — a thrown server-action
// error is redacted in production, so refusals are returned (SIGMA-365).
const setActiveOrg = vi.fn<(orgId: string) => Promise<{ ok: true }>>(async () => ({ ok: true }));
const createOrg = vi.fn<
  (input: { name: string }) => Promise<{ ok: true; orgId: string; name: string }>
>(async ({ name }) => ({ ok: true, orgId: "org_new", name }));
vi.mock("@/server/actions/org", () => ({
  setActiveOrg: (orgId: string) => setActiveOrg(orgId),
  createOrg: (input: { name: string }) => createOrg(input),
  updateOrg: vi.fn(),
}));

import { OrgSwitcher } from "./org-switcher";
import { OrgProvider } from "./org-context";
import { SidebarProvider } from "@/components/ui/sidebar";

// The sidebar's mobile hook subscribes to a media query; jsdom ships no
// matchMedia. Desktop is the only layout these assertions care about.
Object.defineProperty(window, "matchMedia", {
  writable: true,
  value: (query: string) => ({
    matches: false,
    media: query,
    addEventListener: () => {},
    removeEventListener: () => {},
    addListener: () => {},
    removeListener: () => {},
    onchange: null,
    dispatchEvent: () => false,
  }),
});

const ORGS = [
  { id: "org_1", name: "Acme", slug: "acme", plan: "free", serverCount: 2 },
];
const USER = { id: "usr_1", name: "Ada", email: "ada@acme.example" };

function renderSwitcher() {
  return render(
    <OrgProvider orgs={ORGS} activeOrgId="org_1" user={USER}>
      <SidebarProvider>
        <OrgSwitcher />
      </SidebarProvider>
    </OrgProvider>
  );
}

function openMenu() {
  fireEvent.click(screen.getByRole("button", { name: /Acme/ }));
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("OrgSwitcher", () => {
  it('"New organization" opens a create flow', async () => {
    renderSwitcher();
    openMenu();

    const item = screen.getByRole("menuitem", { name: /New organization/ });
    // Not a link into the current org's settings: that page renames THIS org.
    expect(item.getAttribute("href")).toBeNull();

    fireEvent.click(item);

    // A dialog that asks for the new organization's name.
    const dialog = await screen.findByRole("dialog");
    expect(dialog.textContent).toMatch(/New organization/i);
    const field = screen.getByLabelText(/Organization name/i) as HTMLInputElement;
    // Empty, not pre-filled with the org you are already in.
    expect(field.value).toBe("");

    fireEvent.change(field, { target: { value: "Beta Client" } });
    fireEvent.click(screen.getByRole("button", { name: /Create organization/i }));

    await waitFor(() => expect(createOrg).toHaveBeenCalledWith({ name: "Beta Client" }));
    // And the new org becomes the active one, so the user lands inside it.
    await waitFor(() => expect(refresh).toHaveBeenCalled());
  });
});
