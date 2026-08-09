import { redirect } from "next/navigation";
import { getActiveOrgId, getMyOrgs } from "@/server/active-org";
import { getBillingSummary, getServers } from "@/server/queries";
import { cpEnabled, cpGetBilling } from "@/server/cp";
import { reportCpFailure } from "@/server/cp-sync";
import { BillingView } from "@/components/dashboard/billing/billing-view";

export default async function BillingPage() {
  const orgId = await getActiveOrgId();
  if (!orgId) redirect("/login");

  const [billing, servers, myOrgs] = await Promise.all([
    getBillingSummary(orgId),
    getServers(orgId),
    getMyOrgs(),
  ]);
  const orgName = myOrgs.find((o) => o.id === orgId)?.name ?? "your organization";

  const serverItems = servers.map((sv) => ({
    id: sv.id,
    name: sv.name,
    type: sv.type,
    region: sv.region,
    status: sv.status,
    byoVpn: sv.byoVpn,
  }));

  // CP mode: real metered usage + Paddle subscription state (P2-4).
  //
  // A failure here does NOT degrade to the locally computed summary (SIGMA-242).
  // That summary comes from getBillingSummary → getServers, which falls back to
  // the Drizzle mirror when the CP is unreachable — it describes a fleet, and it
  // knows nothing about a subscription. Substituting it produced a page that
  // looked like a finished bill: a "Current monthly cost" that can read €0 off a
  // stale mirror, an "Invoice preview", a "Total due" — and, because the
  // subscription card was simply dropped, no past_due warning and no "update
  // your payment method". The narrow, realistic outage is a CP whose /billing
  // 503s (Paddle not wired) while everything else is healthy, so the mirror sync
  // succeeds and the dashboard-wide banner stays down too. A customer could then
  // lapse and have their servers suspended without one warning in the product.
  //
  // So: report the failure to the banner, and say plainly that we do not know.
  let subscription;
  let summary = billing;
  let cpBillingError: string | null = null;
  if (cpEnabled()) {
    try {
      const b = await cpGetBilling(orgId);
      subscription = {
        configured: b.configured,
        status: b.subscription.status,
        billableUnits: b.billableUnits,
        serverHoursThisMonth: b.serverHoursThisMonth,
        orgId,
      };
      // Prefer the CP's own unit arithmetic — it is what Paddle is charged with,
      // so the preview must never disagree with the invoice over a mirror lag.
      summary = {
        connected: b.connected,
        units: b.units,
        billableUnits: b.billableUnits,
        breakdown: b.breakdown,
        freeTier: b.freeTier,
        unitPrice: b.unitPrice,
        currency: b.currency,
        amount: b.amount,
        isFree: b.billableUnits === 0,
      };
    } catch (err) {
      subscription = undefined;
      cpBillingError = err instanceof Error ? err.message : String(err);
      // The shared "control plane unreachable" banner reads the same store the
      // mirror sync writes; without this the outage was invisible everywhere.
      reportCpFailure(orgId, err);
    }
  }

  return (
    <BillingView
      orgName={orgName}
      // Nothing to render figures from when the CP did not answer — the local
      // summary is deliberately not passed, so mirror numbers cannot leak into
      // a page about money owed.
      billing={cpBillingError ? null : summary}
      servers={serverItems}
      subscription={subscription}
      cpBillingError={cpBillingError}
    />
  );
}
