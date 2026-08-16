import { redirect } from "next/navigation";
import { getActiveOrgId, getMyOrgs } from "@/server/active-org";
import { getServersWithCounts, type ServerWithCount } from "@/server/queries";
import { cpEnabled, cpListServers, cpListResources, cpServerToRow } from "@/server/cp";
import { ServersView } from "@/components/dashboard/servers/servers-view";
import { listClusters } from "@/server/actions/clusters";
import { getClusterEnvironments } from "@/server/queries";
import { getSessionUser, visibleProjects } from "@/server/active-org";

async function cpServersWithCounts(orgId: string): Promise<ServerWithCount[]> {
  const [servers, resources] = await Promise.all([
    cpListServers(orgId),
    cpListResources(orgId).catch(() => []),
  ]);
  const counts = new Map<string, number>();
  for (const r of resources) counts.set(r.serverId, (counts.get(r.serverId) ?? 0) + 1);
  return servers.map((sv) => ({
    ...cpServerToRow(sv),
    resourceCount: counts.get(sv.id) ?? 0,
  }));
}

export default async function ServersPage() {
  const orgId = await getActiveOrgId();
  if (!orgId) redirect("/login");

  const cp = cpEnabled();
  const [servers, myOrgs, clusterData, sessionUser] = await Promise.all([
    // Fall back to the mirror when the control plane cannot be reached
    // (SIGMA-365). These were the only routes that CRASHED during an outage:
    // the layout renders "Control plane unreachable · Showing the last synced
    // state" and directly beneath it this page threw its error boundary, while
    // Overview, Projects, Resources and Billing all degraded politely. Servers
    // is also where Disconnect and Force disconnect live, so the outage took
    // away the controls an operator reaches for during an outage.
    cp
      ? cpServersWithCounts(orgId).catch(() => getServersWithCounts(orgId))
      : getServersWithCounts(orgId),
    getMyOrgs(),
    // Both modes: the clusters panel is where a cluster is built, and hiding it
    // with no control plane made "promote your own servers into a cluster" a
    // capability nobody without hardware could see (SIGMA-215).
    listClusters(orgId),
    getSessionUser(),
  ]);
  const org = myOrgs.find((o) => o.id === orgId);

  // A cluster is created inside an environment, so only offer environments the
  // user can actually see (P2-7 project scoping). Also asked in both modes now:
  // an empty list hides the "New cluster" button, which is how the demo panel
  // would have rendered as a permanent empty state (SIGMA-215).
  const visible = await visibleProjects(sessionUser.id, orgId, org?.role ?? "Developer");
  const environments = await getClusterEnvironments(orgId, visible);

  return (
    <ServersView
      orgId={orgId}
      orgName={org?.name ?? "your organization"}
      orgSlug={org?.slug ?? "org"}
      servers={servers}
      cpMode={cp}
      clusters={clusterData.clusters}
      clusterExcludedKinds={clusterData.excludedKinds}
      clusterEnvironments={environments}
    />
  );
}
