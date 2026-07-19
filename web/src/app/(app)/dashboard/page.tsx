import { redirect } from "next/navigation";
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

  const [servers, billing, resources, myOrgs] = await Promise.all([
    getServers(orgId),
    getBillingSummary(orgId),
    getOrgResources(orgId, visible),
    getMyOrgs(),
  ]);
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
      resources={overviewResources}
      activity={activity}
    />
  );
}
