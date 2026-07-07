import { redirect } from "next/navigation";
import { getActiveOrgId, getMyOrgs } from "@/server/active-org";
import { getBillingSummary, getServers } from "@/server/queries";
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

  return <BillingView orgName={orgName} billing={billing} servers={serverItems} />;
}
