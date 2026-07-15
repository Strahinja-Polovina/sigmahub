import { redirect } from "next/navigation";
import { getActiveOrgId, getMyOrgs } from "@/server/active-org";
import { getServersWithCounts, type ServerWithCount } from "@/server/queries";
import { cpEnabled, cpListServers, cpListResources, cpServerToRow } from "@/server/cp";
import { ServersView } from "@/components/dashboard/servers/servers-view";

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
  const [servers, myOrgs] = await Promise.all([
    cp ? cpServersWithCounts(orgId) : getServersWithCounts(orgId),
    getMyOrgs(),
  ]);
  const org = myOrgs.find((o) => o.id === orgId);

  return (
    <ServersView
      orgId={orgId}
      orgName={org?.name ?? "your organization"}
      orgSlug={org?.slug ?? "org"}
      servers={servers}
      cpMode={cp}
    />
  );
}
