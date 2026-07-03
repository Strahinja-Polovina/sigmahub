import { notFound, redirect } from "next/navigation";
import { getActiveOrgId } from "@/server/active-org";
import { getServer, getServerHosted } from "@/server/queries";
import { ServerDetailView } from "@/components/dashboard/servers/server-detail-view";

export default async function ServerDetailPage({
  params,
}: {
  params: Promise<{ serverId: string }>;
}) {
  const { serverId } = await params;
  const orgId = await getActiveOrgId();
  if (!orgId) redirect("/login");

  const server = await getServer(serverId);
  if (!server || server.orgId !== orgId) notFound();

  const hosted = await getServerHosted(serverId);
  return <ServerDetailView server={server} hosted={hosted} />;
}
