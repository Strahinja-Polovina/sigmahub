"use server";

// Billing (P2-4, Paddle). Summary is member-visible; checkout and the customer
// portal mutate the subscription so they need Org Admin. When Paddle is not
// configured the CP returns an honest not-configured summary / 503.

import { requireMembership, requireOrgAdmin } from "../active-org";
import {
  cpEnabled,
  cpBillingCheckout,
  cpBillingPortal,
  cpGetBilling,
  type CpBillingSummary,
} from "../cp";

function ensureCp() {
  if (!cpEnabled()) {
    throw new Error("Billing requires the control plane (set SIGMAHUB_CP_URL).");
  }
}

export async function getBilling(orgId: string): Promise<CpBillingSummary> {
  ensureCp();
  await requireMembership(orgId);
  return cpGetBilling(orgId);
}

/** Start a Paddle hosted checkout; returns the URL to redirect to. */
export async function startCheckout(orgId: string): Promise<{ checkoutUrl: string }> {
  ensureCp();
  const user = await requireOrgAdmin(orgId);
  return cpBillingCheckout(orgId, { name: user.name, role: "Org Admin" });
}

/** Open the Paddle customer portal (manage payment method / subscription). */
export async function openBillingPortal(orgId: string): Promise<{ portalUrl: string }> {
  ensureCp();
  const user = await requireOrgAdmin(orgId);
  return cpBillingPortal(orgId, { name: user.name, role: "Org Admin" });
}
