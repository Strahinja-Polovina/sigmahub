import { redirect } from "next/navigation";
import { getActiveOrgId, getMyOrgs } from "@/server/active-org";
import { getServersWithCounts } from "@/server/queries";
import { ServersView } from "@/components/dashboard/servers/servers-view";

export default async function ServersPage() {
  const orgId = await getActiveOrgId();
  if (!orgId) redirect("/login");

  const [servers, myOrgs] = await Promise.all([
    getServersWithCounts(orgId),
    getMyOrgs(),
  ]);
  const org = myOrgs.find((o) => o.id === orgId);

  return (
    <ServersView
      orgId={orgId}
      orgName={org?.name ?? "your organization"}
      orgSlug={org?.slug ?? "org"}
      servers={servers}
    />
  );
}
