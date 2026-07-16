import { notFound, redirect } from "next/navigation";
import { getActiveOrgId } from "@/server/active-org";
import { getEnvironment, getProject, getResource, getServer, getServerHosted } from "@/server/queries";
import {
  cpEnabled,
  cpGetServer,
  cpListResources,
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
    const [points, cpResources] = await Promise.all([
      cpServerMetrics(orgId, serverId).then(cpMetricsToPoints),
      cpListResources(orgId).catch(() => []),
    ]);
    // Hosted resources come from the CP; project/env names resolve from the
    // local mirror rows (same ids), falling back to raw ids.
    const hosted = await Promise.all(
      cpResources
        .filter((r) => r.serverId === serverId)
        .map(async (r) => {
          const [project, env, mirror] = await Promise.all([
            getProject(r.projectId),
            getEnvironment(r.environmentId),
            getResource(r.id),
          ]);
          return {
            id: r.id,
            name: r.name,
            kind: r.kind === "mongodb" ? "mongo" : r.kind,
            // CP status.state is authoritative once the P1-2 reconciler
            // populates it; until then fall back to the local mirror's status
            // so the detail page agrees with the project pages.
            status:
              (r.status as { state?: string }).state ??
              mirror?.status ??
              "provisioning",
            projectName: project?.name ?? r.projectId,
            envName: env?.name ?? r.environmentId,
          };
        })
    );
    return (
      <ServerDetailView
        server={cpServerToRow(cpServer)}
        hosted={hosted}
        cpMode
        metricsPoints={points}
        hardening={{
          ready: Boolean(cpServer.ready),
          score: cpServer.hardeningScore ?? null,
          diskEncrypted: cpServer.diskEncrypted ?? null,
          sshLocked: cpServer.sshLocked ?? null,
          distro: cpServer.distro ?? null,
        }}
      />
    );
  }

  const server = await getServer(serverId);
  if (!server || server.orgId !== orgId) notFound();

  const hosted = await getServerHosted(serverId);
  return <ServerDetailView server={server} hosted={hosted} />;
}
