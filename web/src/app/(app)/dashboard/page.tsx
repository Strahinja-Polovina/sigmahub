import { redirect } from "next/navigation";
import { cpEnabled, cpGetBilling } from "@/server/cp";
import { getActiveOrgId, getMyOrgs, requireMembership, visibleProjects } from "@/server/active-org";
import {
  getBillingSummary,
  getOrgResources,
  getServers,
} from "@/server/queries";
import { Overview } from "@/components/dashboard/overview";

export default async function DashboardPage() {
  const orgId = await getActiveOrgId();
  if (!orgId) redirect("/login");

  // P2-7 read scoping: a project-scoped user's overview only counts and lists
  // resources in projects they were granted (SIGMA-75).
  const { user, role } = await requireMembership(orgId);
  const visible = await visibleProjects(user.id, orgId, role);

  const [servers, mirrorBilling, resources, myOrgs] = await Promise.all([
    getServers(orgId),
    getBillingSummary(orgId),
    getOrgResources(orgId, visible),
    getMyOrgs(),
  ]);

  // The control plane owns every billing number (SIGMA-365).
  //
  // getBillingSummary recomputes the charge from the local mirror as
  // `max(0, units - freeTier) * unitPrice`. That applies neither the
  // subscription minimum nor the 24h high-water mark, both of which the CP's
  // BillableQuantity does — so a customer with 3 units who has just subscribed
  // is billed €5/mo by Paddle and told "€0 /mo · Free tier" by the first screen
  // they see after every login. That is the most common new subscriber, since
  // subscribing at exactly the tier is what this round unblocked.
  //
  // The Billing page already refuses to publish this number for the same reason
  // and says so in a comment; the Overview published it unconditionally. In CP
  // mode the tile now shows the CP's figure, or nothing at all — a dash the user
  // can go and resolve is better than a confident wrong number about money.
  let billing = mirrorBilling;
  let billingUnavailable = false;
  if (cpEnabled()) {
    try {
      const b = await cpGetBilling(orgId);
      billing = {
        ...mirrorBilling,
        units: b.units,
        billedUnits: b.billedUnits,
        billableUnits: b.billableUnits,
        freeTier: b.freeTier,
        unitPrice: b.unitPrice,
        currency: b.currency,
        amount: b.amount,
        isFree: b.billableUnits === 0,
      };
    } catch {
      billingUnavailable = true;
    }
  }
  const orgName = myOrgs.find((o) => o.id === orgId)?.name ?? "your organization";

  const connectedServers = servers.filter((s) => s.status !== "provisioning").length;
  const runningResources = resources.filter((r) => r.status === "running").length;
  const activeDeploys = resources.filter(
    (r) => r.latestDeploy?.status === "running"
  ).length;

  const activity = resources
    .filter((r) => r.latestDeploy)
    .sort(
      (a, b) =>
        new Date(b.latestDeploy!.startedAt).getTime() -
        new Date(a.latestDeploy!.startedAt).getTime()
    )
    .slice(0, 6)
    .map((r) => ({
      id: r.latestDeploy!.id,
      resourceName: r.name,
      author: r.latestDeploy!.author,
      sha: r.latestDeploy!.sha,
      durationSec: r.latestDeploy!.durationSec,
      status: r.latestDeploy!.status,
    }));

  const overviewResources = resources.map((r) => ({
    id: r.id,
    name: r.name,
    kind: r.kind,
    status: r.status,
    projectName: r.projectName,
    envName: r.envName,
    lastDeployAt: r.lastDeployAt,
  }));

  return (
    <Overview
      orgName={orgName}
      connectedServers={connectedServers}
      totalServers={servers.length}
      runningResources={runningResources}
      activeDeploys={activeDeploys}
      billing={billing}
      billingUnavailable={billingUnavailable}
      resources={overviewResources}
      activity={activity}
    />
  );
}
