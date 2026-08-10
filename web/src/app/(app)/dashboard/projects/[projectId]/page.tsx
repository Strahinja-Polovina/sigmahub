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
  cpListDeployRequests,
  type CpGitConnection,
  type CpBranchMap,
} from "@/server/cp";
import { isInstallationId } from "@/lib/github-app";
import { WIZARD_RESUME_PARAM, WIZARD_RESUME_VALUE } from "@/lib/wizard/resume";
import { listClusters } from "@/server/actions/clusters";
import { ProjectDetailView } from "@/components/dashboard/projects/project-detail-view";
import type {
  GitAppInfo,
  GitConnectionPanel,
  PushActivity,
} from "@/components/dashboard/projects/project-git-panel";

/** How many rows the panel shows. */
const PUSHES_SHOWN = 5;
/** How many rows to ask the CP for per connection. More than PUSHES_SHOWN
 *  because PR hooks share the table with pushes and are dropped below, so a
 *  window of exactly five could render fewer than five deploys. */
const PUSHES_PER_CONNECTION = 20;

/** Recent pushes for this project's repositories, newest first.
 *
 *  A push that resolved to nothing used to be indistinguishable from one that
 *  deployed, so "I pushed and nothing happened" had no answer anywhere.
 *
 *  SIGMA-330: this asks the control plane per connection. It used to request the
 *  org's deploy requests — a 50-row window shared by every repository in the org
 *  — and filter it down to this project's connections here. In an org with four
 *  active repos, one repo's CI pushing 60 times in an afternoon owns that whole
 *  window, so an operator opening a different project saw an EMPTY panel for a
 *  repo they had pushed to twenty minutes earlier. The panel exists precisely to
 *  answer "I pushed, why is nothing happening?" (migration 0052), and answering
 *  it with silence reads as "the webhook never arrived". Scoped in SQL, each
 *  repo's history is bounded by its own volume. */
async function loadPushes(
  orgId: string,
  connections: GitConnectionPanel[]
): Promise<PushActivity[]> {
  if (!cpEnabled() || connections.length === 0) return [];
  // Per connection, and resilient per connection: one repo's failed read (a
  // concurrent delete → 404) must not blank the other repos' pushes.
  const perConnection = await Promise.all(
    connections.map((c) =>
      cpListDeployRequests(orgId, c.connection.id, PUSHES_PER_CONNECTION).catch(() => [])
    )
  );
  return perConnection
    .flat()
    .filter((d) => d.kind !== "pr_hook")
    // Each connection's rows arrive newest-first; merging several needs the
    // order re-established across them.
    .sort((a, b) => Date.parse(b.createdAt) - Date.parse(a.createdAt))
    .slice(0, PUSHES_SHOWN)
    .map((d) => ({
      id: d.id,
      ref: d.ref,
      sha: d.sha,
      status: d.status,
      deploymentsCreated: d.deploymentsCreated ?? 0,
      detail: d.detail,
      createdAt: d.createdAt,
    }));
}

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

  const [panels, servers, gitConnections, gitApp, clusterData] = await Promise.all([
    getEnvironmentPanels(projectId),
    getServers(orgId),
    loadGitConnections(orgId, projectId),
    loadGitApp(orgId),
    // A cluster is a deploy TARGET, and the wizard could not offer one because
    // nothing loaded them here (SIGMA-210). Asked in both modes since
    // SIGMA-215 — listClusters answers for the demo tables too, and the
    // exclusion list it returns is the catalog's rather than an empty array,
    // which the control plane reads as "a cluster hosts every kind".
    listClusters(orgId),
  ]);
  // What recent pushes to this project's repositories actually produced. The
  // org-wide list is filtered to this project's connections; a CP failure just
  // hides the section rather than breaking the page.
  const pushes = await loadPushes(orgId, gitConnections);
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
      pushes={pushes}
      gitApp={gitApp}
      pendingInstallationId={pendingInstallationId}
      projectMembers={projectMembers}
      canManageMembers={canManageMembers}
      cpMode={cpEnabled()}
      clusters={clusterData.clusters}
      clusterExcludedKinds={clusterData.excludedKinds}
      resumeWizard={query[WIZARD_RESUME_PARAM] === WIZARD_RESUME_VALUE}
    />
  );
}
