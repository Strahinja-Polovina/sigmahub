import { redirect } from "next/navigation";
import { getActiveOrgId, getMyOrgs } from "@/server/active-org";
import { getServersWithCounts, type ServerWithCount } from "@/server/queries";
import { cpEnabled, cpListServers, cpServerToRow } from "@/server/cp";
import { ServersView } from "@/components/dashboard/servers/servers-view";

export default async function ServersPage() {
  const orgId = await getActiveOrgId();
  if (!orgId) redirect("/login");

  const cp = cpEnabled();
  const [servers, myOrgs] = await Promise.all([
    cp
      ? cpListServers(orgId).then(
          // CP servers carry no local scheduling data yet → 0 resources.
          (list): ServerWithCount[] =>
            list.map((sv) => ({ ...cpServerToRow(sv), resourceCount: 0 }))
        )
      : getServersWithCounts(orgId),
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
