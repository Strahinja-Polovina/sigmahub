import { redirect } from "next/navigation";
import { eq } from "drizzle-orm";
import {
  getActiveOrgId,
  getSessionUser,
  projectGrants,
  requireMembership,
} from "@/server/active-org";
import { effectiveProjectRole, roleAtLeast } from "@/lib/rbac";
import { db } from "@/server/db";
import * as dbs from "@/server/db/schema";
import { getEnvironmentPanels, getMembers, getProject, getServers } from "@/server/queries";
import type { ProjectMemberRow } from "@/components/dashboard/projects/project-members-panel";
import {
  cpEnabled,
  cpGitAppInfo,
  cpListGitConnectionsWithMaps,
  type CpGitConnection,
  type CpBranchMap,
} from "@/server/cp";
import { isInstallationId } from "@/lib/github-app";
import { ProjectDetailView } from "@/components/dashboard/projects/project-detail-view";
import type { GitAppInfo, GitConnectionPanel } from "@/components/dashboard/projects/project-git-panel";

/** Fetch the project's Git connections + branch routes (CP mode only). A CP
 *  failure must not break the page — degrade to an empty panel. */
async function loadGitConnections(orgId: string, projectId: string): Promise<GitConnectionPanel[]> {
  if (!cpEnabled()) return [];
  try {
    const rows = await cpListGitConnectionsWithMaps(orgId, projectId);
    return rows.map((r: { connection: CpGitConnection; branchMaps: CpBranchMap[] }) => ({
      connection: {
        id: r.connection.id,
        repoFullName: r.connection.repoFullName,
        installationId: r.connection.installationId || undefined,
        previewsEnabled: r.connection.previewsEnabled,
        previewServerId: r.connection.previewServerId,
      },
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

/** GitHub App availability for the Install/Link buttons; a CP failure just
 *  hides them. */
async function loadGitApp(orgId: string): Promise<GitAppInfo | undefined> {
  if (!cpEnabled()) return undefined;
  try {
    return await cpGitAppInfo(orgId);
  } catch {
    return undefined;
  }
}

export default async function ProjectDetailPage({
  params,
  searchParams,
}: {
  params: Promise<{ projectId: string }>;
  searchParams: Promise<Record<string, string | string[] | undefined>>;
}) {
  const { projectId } = await params;
  const query = await searchParams;
  const orgId = await getActiveOrgId();
  if (!orgId) redirect("/login");

  const project = await getProject(projectId);
  // Guard: missing project, or one that belongs to a different org.
  if (!project || project.orgId !== orgId) {
    return <ProjectDetailView project={null} panels={[]} orgServers={[]} />;
  }

  // P2-7: project-scoped users only see granted projects; the effective role
  // decides whether the members panel is editable.
  const sessionUser = await getSessionUser();
  const { role: orgRole, scoped } = await requireMembership(orgId);
  const grants = await projectGrants(sessionUser.id, orgId);
  const myEffectiveRole = effectiveProjectRole(orgRole, grants.get(projectId), scoped || grants.size > 0);
  if (!myEffectiveRole) {
    return <ProjectDetailView project={null} panels={[]} orgServers={[]} />;
  }
  const canManageMembers = roleAtLeast(myEffectiveRole, "Project Admin");

  const [orgMembers, grantRows] = await Promise.all([
    getMembers(orgId),
    db
      .select({ userId: dbs.projectMemberships.userId, role: dbs.projectMemberships.role })
      .from(dbs.projectMemberships)
      .where(eq(dbs.projectMemberships.projectId, projectId)),
  ]);
  const grantByUser = new Map(grantRows.map((g) => [g.userId, g.role]));
  const projectMembers: ProjectMemberRow[] = orgMembers.map((m) => ({
    userId: m.id,
    name: m.name,
    email: m.email,
    orgRole: m.role,
    grantedRole: grantByUser.get(m.id) ?? null,
  }));

  // Bounced back from the GitHub App install flow (SIGMA-55): the connect
  // dialog opens pre-filled with this installation.
  const rawInstallation = typeof query.installation_id === "string" ? query.installation_id : undefined;
  const pendingInstallationId = isInstallationId(rawInstallation) ? rawInstallation : undefined;

  const [panels, servers, gitConnections, gitApp] = await Promise.all([
    getEnvironmentPanels(projectId),
    getServers(orgId),
    loadGitConnections(orgId, projectId),
    loadGitApp(orgId),
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
      gitEnabled={cpEnabled()}
      gitConnections={gitConnections}
      gitApp={gitApp}
      pendingInstallationId={pendingInstallationId}
      projectMembers={projectMembers}
      canManageMembers={canManageMembers}
      cpMode={cpEnabled()}
    />
  );
}
