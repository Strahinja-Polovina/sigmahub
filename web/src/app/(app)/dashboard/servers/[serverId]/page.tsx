import { notFound, redirect } from "next/navigation";
import { getActiveOrgId } from "@/server/active-org";
import { getServer, getServerHosted } from "@/server/queries";
import {
  cpEnabled,
  cpGetServer,
  cpMetricsToPoints,
  cpServerMetrics,
  cpServerToRow,
} from "@/server/cp";
import { ServerDetailView } from "@/components/dashboard/servers/server-detail-view";

export default async function ServerDetailPage({
  params,
}: {
  params: Promise<{ serverId: string }>;
}) {
  const { serverId } = await params;
  const orgId = await getActiveOrgId();
  if (!orgId) redirect("/login");

  if (cpEnabled()) {
    const cpServer = await cpGetServer(orgId, serverId);
    if (!cpServer) notFound();
    const points = cpMetricsToPoints(await cpServerMetrics(orgId, serverId));
    return (
      <ServerDetailView
        server={cpServerToRow(cpServer)}
        hosted={[]}
        cpMode
        metricsPoints={points}
      />
    );
  }

  const server = await getServer(serverId);
  if (!server || server.orgId !== orgId) notFound();

  const hosted = await getServerHosted(serverId);
  return <ServerDetailView server={server} hosted={hosted} />;
}
