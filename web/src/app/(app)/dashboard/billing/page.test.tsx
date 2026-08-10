// @vitest-environment jsdom
//
// What Billing is allowed to show when the control plane did not answer
// (SIGMA-242).
//
// The page has two sources for the same numbers: the control plane's own unit
// arithmetic — which is what Paddle is charged with — and getBillingSummary,
// computed locally from getServers, which falls back to the Drizzle mirror when
// the CP is unreachable. The second is a preview of a fleet, never an invoice.
//
// The failure that matters is the narrow one: the CP is healthy enough that the
// mirror sync succeeds (so the dashboard-wide "control plane unreachable" banner
// stays down) but /billing 503s. The page then rendered a complete, confident
// billing page from mirror numbers — no past-due warning, no "update your
// payment method", a "Current monthly cost" that can read €0 — to a customer
// whose subscription is actually past_due. There is no other place in the
// product that would have told them.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

const cpEnabled = vi.fn(() => true);
const cpGetBilling = vi.fn();
const reportCpFailure = vi.fn();

vi.mock("next/navigation", () => ({
  redirect: (to: string) => {
    throw new Error(`unexpected redirect to ${to}`);
  },
}));
vi.mock("@/server/active-org", () => ({
  getActiveOrgId: async () => "org_1",
  getMyOrgs: async () => [{ id: "org_1", name: "Acme", slug: "acme", plan: "pro", role: "Owner" }],
}));
vi.mock("@/server/queries", () => ({
  // The mirror-derived summary: what the page must NOT present as a bill.
  getBillingSummary: async () => ({
    connected: 2,
    units: 6,
    billableUnits: 3,
    breakdown: [{ type: "app", count: 2, weight: 1, units: 2 }],
    freeTier: 3,
    unitPrice: 25,
    currency: "EUR",
    amount: 75,
    isFree: false,
  }),
  getServers: async () => [
    {
      id: "srv_1",
      name: "web-1",
      type: "app",
      region: "eu-west-1",
      status: "running",
      byoVpn: false,
    },
  ],
}));
vi.mock("@/server/cp-sync", () => ({
  reportCpFailure: (orgId: string, err: unknown) => reportCpFailure(orgId, err),
}));
vi.mock("@/server/cp", () => ({
  cpEnabled: () => cpEnabled(),
  cpGetBilling: (orgId: string) => cpGetBilling(orgId),
}));

import BillingPage from "./page";

beforeEach(() => {
  cpEnabled.mockReturnValue(true);
  cpGetBilling.mockReset();
  reportCpFailure.mockReset();
});
afterEach(cleanup);

describe("BillingPage", () => {
  it("a CP billing failure shows an unavailable state, not mirror numbers", async () => {
    cpGetBilling.mockRejectedValueOnce(new Error("503 billing not configured"));

    render(await BillingPage());

    // Not one monetary figure: every one of them would be a claim about money
    // owed, sourced from a mirror that has no idea what the invoice says.
    expect(document.body.textContent).not.toContain("€");
    expect(screen.queryByText(/Current monthly cost/i)).toBeNull();
    expect(screen.queryByText(/Invoice preview/i)).toBeNull();

    // The page says what happened, in its own words.
    expect(screen.getByText(/couldn’t reach the control plane/i)).toBeTruthy();

    // And the dashboard-wide banner is told, so the outage is visible outside
    // this page too.
    expect(reportCpFailure).toHaveBeenCalledTimes(1);
    expect(reportCpFailure.mock.calls[0][0]).toBe("org_1");
  });

  it("renders the control plane's own figures when it answers", async () => {
    cpGetBilling.mockResolvedValueOnce({
      configured: true,
      subscription: { status: "past_due" },
      connected: 2,
      units: 8,
      billableUnits: 5,
      breakdown: [{ type: "app", count: 2, weight: 1, units: 2 }],
      freeTier: 3,
      unitPrice: 25,
      currency: "EUR",
      amount: 125,
      serverHoursThisMonth: 720,
    });

    render(await BillingPage());

    expect(screen.getByText(/Payment past due/i)).toBeTruthy();
    // The CP's arithmetic (8 units → €125), not the mirror's (6 units → €75).
    expect(document.body.textContent).toContain("€125");
    expect(document.body.textContent).toContain("bills as 8 units");
    expect(reportCpFailure).not.toHaveBeenCalled();
  });

  it("demo mode still renders the local preview", async () => {
    cpEnabled.mockReturnValue(false);

    render(await BillingPage());

    expect(document.body.textContent).toContain("€75");
    expect(cpGetBilling).not.toHaveBeenCalled();
  });
});
