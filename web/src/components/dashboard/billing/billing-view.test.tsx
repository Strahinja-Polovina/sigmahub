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

describe("BillingView subscription card", () => {
  // SIGMA-294: the CP collapsed `paused` into `canceled`, so a subscription an
  // Org Admin paused for a month rendered "Canceled" with a live Subscribe
  // button — the only affordance on the card. Clicking it created a SECOND
  // Paddle subscription, and the org was charged twice the moment the first
  // resumed. Paused now has its own status and sends the admin to the portal.
  it("offers Resume, not Subscribe, while billing is paused", () => {
    render(
      <BillingView
        orgName="Acme"
        billing={billing}
        servers={servers}
        subscription={{ ...subscription, status: "paused" }}
      />
    );
    expect(screen.getByText("Paused")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /^Subscribe$/ })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: /Resume subscription/ }));
    expect(startCheckout).not.toHaveBeenCalled();
  });

  it("still offers Subscribe once the subscription is genuinely canceled", () => {
    render(
      <BillingView
        orgName="Acme"
        billing={billing}
        servers={servers}
        subscription={{ ...subscription, status: "canceled" }}
      />
    );
    expect(screen.getByRole("button", { name: /^Subscribe$/ })).toBeTruthy();
  });

  // SIGMA-365. The closed loop for the customer who most needs to pay.
  //
  // At EXACTLY the free tier, billableUnits is 0 — nothing is chargeable yet —
  // and that is the same moment the free-tier gate starts refusing new servers,
  // with a message telling the customer to subscribe from this page. They
  // arrived and found the button greyed out, labelled "Within free tier".
  // Refused growth, refused payment, no way forward from inside the product.
  //
  // The control plane's checkout handler was fixed to accept this case in the
  // previous round; this predicate was the only thing still holding them out,
  // which is why the fix looked complete and was not.
  it("offers Subscribe to an org sitting at exactly the free tier", () => {
    render(
      <BillingView
        orgName="Acme"
        billing={billing}
        servers={servers}
        subscription={{
          ...subscription,
          status: "none",
          billableUnits: 0, // nothing chargeable yet...
          billedUnits: 3, // ...but the fleet is at the ceiling
          freeTier: 3,
        }}
      />
    );
    const btn = screen.getByRole("button", { name: /^Subscribe$/ }) as HTMLButtonElement;
    expect(btn.disabled).toBe(false);
  });

  it("keeps checkout closed while the org is genuinely below the tier", () => {
    // Subscribing here bills the minimum quantity for capacity that is free, and
    // nothing is refusing them anything yet — so the button stays off.
    render(
      <BillingView
        orgName="Acme"
        billing={billing}
        servers={servers}
        subscription={{
          ...subscription,
          status: "none",
          billableUnits: 0,
          billedUnits: 1,
          freeTier: 3,
        }}
      />
    );
    expect(screen.queryByRole("button", { name: /^Subscribe$/ })).toBeNull();
    expect(screen.getByRole("button", { name: /Within free tier/ })).toBeTruthy();
  });

  it("still offers Subscribe to an org already over the tier", () => {
    render(
      <BillingView
        orgName="Acme"
        billing={billing}
        servers={servers}
        subscription={{ ...subscription, status: "none", billableUnits: 5, billedUnits: 8, freeTier: 3 }}
      />
    );
    const btn = screen.getByRole("button", { name: /^Subscribe$/ }) as HTMLButtonElement;
    expect(btn.disabled).toBe(false);
  });
});
