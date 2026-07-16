import { redirect } from "next/navigation";
import { getActiveOrgId } from "@/server/active-org";
import { getEnvironmentPanels, getProject, getServers } from "@/server/queries";
import { cpEnabled, cpListGitConnectionsWithMaps, type CpGitConnection, type CpBranchMap } from "@/server/cp";
import { ProjectDetailView } from "@/components/dashboard/projects/project-detail-view";
import type { GitConnectionPanel } from "@/components/dashboard/projects/project-git-panel";

/** Fetch the project's Git connections + branch routes (CP mode only). A CP
 *  failure must not break the page — degrade to an empty panel. */
async function loadGitConnections(orgId: string, projectId: string): Promise<GitConnectionPanel[]> {
  if (!cpEnabled()) return [];
  try {
    const rows = await cpListGitConnectionsWithMaps(orgId, projectId);
    return rows.map((r: { connection: CpGitConnection; branchMaps: CpBranchMap[] }) => ({
      connection: { id: r.connection.id, repoFullName: r.connection.repoFullName },
      branchMaps: r.branchMaps.map((m) => ({
        id: m.id,
        branch: m.branch,
        environmentId: m.environmentId,
        policy: m.policy,
        lastSha: m.lastSha,
      })),
    }));
  } catch {
    return [];
  }
}

export default async function ProjectDetailPage({
  params,
}: {
  params: Promise<{ projectId: string }>;
}) {
  const { projectId } = await params;
  const orgId = await getActiveOrgId();
  if (!orgId) redirect("/login");

  const project = await getProject(projectId);
  // Guard: missing project, or one that belongs to a different org.
  if (!project || project.orgId !== orgId) {
    return <ProjectDetailView project={null} panels={[]} orgServers={[]} />;
  }

  const [panels, servers, gitConnections] = await Promise.all([
    getEnvironmentPanels(projectId),
    getServers(orgId),
    loadGitConnections(orgId, projectId),
  ]);
  const orgServers = servers.map((sv) => ({
    id: sv.id,
    name: sv.name,
    type: sv.type,
    region: sv.region,
    status: sv.status,
  }));
  return (
    <ProjectDetailView
      project={project}
      panels={panels}
      orgServers={orgServers}
      orgId={orgId}
      gitConnections={gitConnections}
    />
  );
}
