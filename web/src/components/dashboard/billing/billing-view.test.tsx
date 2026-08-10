// @vitest-environment jsdom
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

const openBillingPortal = vi.fn(async () => ({ portalUrl: "https://paddle.test/portal/abc" }));
const startCheckout = vi.fn(async () => ({ checkoutUrl: "https://paddle.test/checkout/abc" }));
vi.mock("@/server/actions/billing", () => ({
  openBillingPortal: (...a: unknown[]) => openBillingPortal(...(a as [])),
  startCheckout: (...a: unknown[]) => startCheckout(...(a as [])),
}));

import { BillingView } from "./billing-view";

const billing = {
  connected: 2,
  units: 3,
  billableUnits: 2,
  breakdown: [],
  freeTier: 1,
  unitPrice: 10,
  currency: "EUR",
  amount: 20,
  isFree: false,
};

const servers = [
  {
    id: "srv_1",
    name: "edge-1",
    type: "app",
    region: "eu-west-1",
    status: "running",
    byoVpn: false,
  },
];

const subscription = {
  configured: true,
  status: "active",
  billableUnits: 2,
  serverHoursThisMonth: 720,
  orgId: "org_1",
};

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("BillingView invoices", () => {
  it("Download invoice opens the billing portal", async () => {
    render(
      <BillingView
        orgName="Acme"
        billing={billing}
        servers={servers}
        subscription={subscription}
      />
    );

    fireEvent.click(screen.getByRole("button", { name: /Download invoice/ }));

    // Paddle is the merchant of record and holds the actual invoice. The button
    // used to be `toast.success("Invoice download started")` and made no request
    // at all (SIGMA-239).
    await waitFor(() => expect(openBillingPortal).toHaveBeenCalledWith("org_1"));
  });

  it("offers no invoice download when payments are not configured", () => {
    render(<BillingView orgName="Acme" billing={billing} servers={servers} />);
    expect(screen.queryByRole("button", { name: /Download invoice/ })).toBeNull();
  });
});
