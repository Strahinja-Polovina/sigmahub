import { redirect } from "next/navigation";
import { getActiveOrgId, getMyOrgs } from "@/server/active-org";
import { getServersWithCounts, type ServerWithCount } from "@/server/queries";
import { cpEnabled, cpListServers, cpListResources, cpServerToRow } from "@/server/cp";
import { ServersView } from "@/components/dashboard/servers/servers-view";
import { listClusters } from "@/server/actions/clusters";
import { getCommandIndex } from "@/server/queries";
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
    cp ? cpServersWithCounts(orgId) : getServersWithCounts(orgId),
    getMyOrgs(),
    cp ? listClusters(orgId) : Promise.resolve({ clusters: [], excludedKinds: [] }),
    getSessionUser(),
  ]);
  const org = myOrgs.find((o) => o.id === orgId);

  // A cluster is created inside an environment, so only offer environments the
  // user can actually see (P2-7 project scoping).
  const visible = await visibleProjects(sessionUser.id, orgId, org?.role ?? "Developer");
  const index = cp
    ? await getCommandIndex(orgId, visible)
    : { environments: [] as { id: string; name: string; projectName: string }[] };
  const environments = index.environments.map((e) => ({
    id: e.id,
    name: e.name,
    projectName: e.projectName,
  }));

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
