"use server";

import { revalidatePath } from "next/cache";
import {
  requireMembership,
  requireProjectRole,
  requireOrgAdmin,
  assertProjectVisible,
} from "../active-org";
import { writeAudit } from "../audit";
import {
  cpEnabled,
  cpDetectRepo,
  cpConnectRepo,
  cpListGitConnections,
  cpSetBranchMap,
  cpDeleteBranchMap,
  cpPromoteBranch,
  cpDisconnectRepo,
  cpSetPreviews,
  cpListPreviews,
  cpGetGitConnection,
  cpGitAppInfo,
  cpLinkInstallation,
  cpGetGitIntegration,
  cpConnectGitIntegration,
  cpDisconnectGitIntegration,
  cpListGitRepos,
  cpSelectGitRepo,
  type CpGitAppInfo,
  type CpDetected,
  type CpGitConnection,
  type CpBranchMap,
  type CpPreviewEnvironment,
  type CpGitIntegration,
  type CpGitHubInstallation,
  type CpRepoList,
} from "../cp";

/** Git integration is a control-plane feature; the demo path has no webhook
 *  receiver or provider connection to back it. */
function ensureCp() {
  if (!cpEnabled()) {
    throw new Error("Git integration requires the control plane (set SIGMAHUB_CP_URL).");
  }
}

/** Resolve a git connection to its REAL project (from the CP, not the
 *  client-supplied projectId) and require Project Admin there — closing the
 *  cross-project IDOR where a scoped Project Admin of project A passed A's
 *  projectId but B's connectionId (SIGMA-93). Returns the actor, the resolved
 *  project, and the connection's branch maps (for map-addressed callers). */
async function requireConnectionAdmin(orgId: string, connectionId: string) {
  const { connection, branchMaps } = await cpGetGitConnection(orgId, connectionId);
  const { user, role } = await requireProjectRole(orgId, connection.projectId, "Project Admin");
  return { user, role, projectId: connection.projectId, branchMaps };
}

/** Preview the deploy config sigmahub detects for a repo. Read-only; any member
 *  can run it to see whether a repo is deployable before connecting.
 *
 *  A CP/GitHub failure (bad token, rate limit, outage) is returned as a
 *  non-deployable result with the real reason instead of being thrown:
 *  Next.js redacts thrown server-action error messages in production, so a
 *  throw would reach the user as the useless "Server Components render"
 *  digest message rather than anything actionable. */
export async function detectRepo(input: {
  orgId: string;
  repoFullName: string;
  installationId?: string;
  token?: string;
}): Promise<CpDetected> {
  ensureCp();
  const { user, role } = await requireMembership(input.orgId);
  try {
    return await cpDetectRepo(
      input.orgId,
      { repoFullName: input.repoFullName.trim(), installationId: input.installationId, token: input.token },
      { name: user.name, role }
    );
  } catch (err) {
    // `unreadable`, not `deployable: false`. The two are different facts and
    // the wizard acts on them differently: "this repo says nothing about how to
    // build itself" invites the user to commit a Dockerfile, which is exactly
    // the wrong advice when what actually happened is a 500, a rate limit or an
    // expired token — the repo may already have one we simply could not read.
    return {
      hasDockerfile: false,
      hasCompose: false,
      ports: [],
      env: [],
      healthCheck: { type: "tcp", intervalSec: 10, source: "default" },
      deployable: false,
      unreadable: true,
      reason: err instanceof Error ? err.message : "Could not read the repository. Please try again.",
    };
  }
}

/** Connect a repo to a project. Project Admin+ — it stores a provider token and
 *  wires push-to-deploy. The CP rejects an undeployable repo (thrown here). */
export async function connectRepo(input: {
  orgId: string;
  projectId: string;
  repoFullName: string;
  installationId?: string;
  token?: string;
}): Promise<CpGitConnection> {
  ensureCp();
  const { user, role } = await requireProjectRole(input.orgId, input.projectId, "Project Admin");
  const repo = input.repoFullName.trim();
  if (!repo.includes("/")) throw new Error("Enter the repository as owner/name.");
  const conn = await cpConnectRepo(
    input.orgId,
    { projectId: input.projectId, repoFullName: repo, installationId: input.installationId, token: input.token },
    { name: user.name, role }
  );
  await writeAudit({ orgId: input.orgId, actor: user.name, action: "Connected Git repo", target: repo });
  revalidatePath(`/dashboard/projects/${input.projectId}`);
  return conn;
}

/** One-call repo wiring for the deploy wizard: connect the repo to the project
 *  (reusing an existing org connection instead of failing on the conflict) and
 *  map a branch to the environment — which server-side also (re)registers the
 *  push webhook and enqueues the initial deploy of the branch head. Returns a
 *  readable summary instead of throwing: Next.js redacts thrown server-action
 *  messages in production. */
export async function wireRepoToEnvironment(input: {
  orgId: string;
  projectId: string;
  repoFullName: string;
  token?: string;
  /** Installation the repo was picked from (org-level GitHub integration). */
  installationId?: string;
  branch: string;
  environmentId: string;
}): Promise<
  | { ok: true; initialDeploy: boolean; webhookRegistered: boolean }
  | { ok: false; error: string }
> {
  ensureCp();
  const { user, role } = await requireProjectRole(input.orgId, input.projectId, "Project Admin");
  const repo = input.repoFullName.trim();
  try {
    let conn: CpGitConnection | undefined;
    try {
      // Repo picked from the org integration: selecting is idempotent by
      // (project, repo), so a second resource on the same repo reuses the
      // connection instead of relying on an "already connected" error string.
      if (!input.token) {
        conn = await cpSelectGitRepo(
          input.orgId,
          {
            projectId: input.projectId,
            repoFullName: repo,
            installationId: input.installationId,
          },
          { name: user.name, role }
        );
      } else {
        conn = await cpConnectRepo(
          input.orgId,
          { projectId: input.projectId, repoFullName: repo, token: input.token },
          { name: user.name, role }
        );
      }
      await writeAudit({ orgId: input.orgId, actor: user.name, action: "Connected Git repo", target: repo });
    } catch (err) {
      // A repo connects once per org — reuse the existing connection so the
      // wizard's second run (or a second resource on the same repo) still
      // maps the branch and triggers the initial deploy.
      const msg = err instanceof Error ? err.message : "";
      if (!msg.toLowerCase().includes("already connected")) throw err;
      const existing = await cpListGitConnections(input.orgId);
      conn = existing.find((c) => c.repoFullName.toLowerCase() === repo.toLowerCase());
      if (!conn) throw err;
    }
    const map = await cpSetBranchMap(
      input.orgId,
      conn.id,
      { branch: input.branch.trim(), environmentId: input.environmentId, policy: "auto" },
      { name: user.name, role }
    );
    await writeAudit({
      orgId: input.orgId,
      actor: user.name,
      action: `Mapped branch ${input.branch} (auto)`,
      target: input.environmentId,
    });
    revalidatePath(`/dashboard/projects/${input.projectId}`);
    return {
      ok: true,
      initialDeploy: Boolean(map.initialDeploy),
      webhookRegistered: Boolean(
        (map as { webhookRegistered?: boolean }).webhookRegistered ?? conn.webhookRegistered
      ),
    };
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : "Could not wire the repository." };
  }
}

/** Map a branch to an environment with a deploy policy (auto | manual). */
export async function setBranchMapping(input: {
  orgId: string;
  projectId: string;
  connectionId: string;
  branch: string;
  environmentId: string;
  policy: "auto" | "manual";
  /** Build this branch on a dedicated server instead of the deploy target. */
  buildServerId?: string;
}): Promise<CpBranchMap> {
  ensureCp();
  // Authorize on the connection's real project (SIGMA-93), not the client's.
  const { user, role } = await requireConnectionAdmin(input.orgId, input.connectionId);
  const branch = input.branch.trim();
  if (!branch) throw new Error("Branch is required.");
  const m = await cpSetBranchMap(
    input.orgId,
    input.connectionId,
    {
      branch,
      environmentId: input.environmentId,
      policy: input.policy,
      buildServerId: input.buildServerId,
    },
    { name: user.name, role }
  );
  await writeAudit({
    orgId: input.orgId,
    actor: user.name,
    action: `Mapped branch ${branch} (${input.policy})`,
    target: input.environmentId,
  });
  revalidatePath(`/dashboard/projects/${input.projectId}`);
  return m;
}

/** Promote a manual branch's last-seen commit to a deploy. */
export async function promoteBranch(input: {
  orgId: string;
  projectId: string;
  connectionId: string;
  mapId: string;
  branch: string;
}): Promise<void> {
  ensureCp();
  // Authorize on the connection's real project and confirm the map belongs to
  // that connection, so a map from another project can't be promoted (SIGMA-93).
  const { user, role, branchMaps } = await requireConnectionAdmin(input.orgId, input.connectionId);
  if (!branchMaps.some((m) => m.id === input.mapId)) {
    throw new Error("Branch mapping not found.");
  }
  await cpPromoteBranch(input.orgId, input.mapId, { name: user.name, role });
  await writeAudit({
    orgId: input.orgId,
    actor: user.name,
    action: `Promoted branch ${input.branch}`,
    target: input.mapId,
  });
  revalidatePath(`/dashboard/projects/${input.projectId}`);
}

export async function removeBranchMapping(input: {
  orgId: string;
  projectId: string;
  connectionId: string;
  mapId: string;
}): Promise<void> {
  ensureCp();
  const { user, role, branchMaps } = await requireConnectionAdmin(input.orgId, input.connectionId);
  if (!branchMaps.some((m) => m.id === input.mapId)) {
    throw new Error("Branch mapping not found.");
  }
  await cpDeleteBranchMap(input.orgId, input.mapId, { name: user.name, role });
  await writeAudit({ orgId: input.orgId, actor: user.name, action: "Removed branch mapping", target: input.mapId });
  revalidatePath(`/dashboard/projects/${input.projectId}`);
}

export async function disconnectRepo(input: {
  orgId: string;
  projectId: string;
  connectionId: string;
  repoFullName: string;
}): Promise<void> {
  ensureCp();
  const { user, role } = await requireConnectionAdmin(input.orgId, input.connectionId);
  await cpDisconnectRepo(input.orgId, input.connectionId, { name: user.name, role });
  await writeAudit({
    orgId: input.orgId,
    actor: user.name,
    action: "Disconnected Git repo",
    target: input.repoFullName,
  });
  revalidatePath(`/dashboard/projects/${input.projectId}`);
}

/** Toggle per-PR preview environments on a connection (P1-12). Enabling
 *  designates the server ephemeral preview resources land on. */
export async function setPreviews(input: {
  orgId: string;
  projectId: string;
  connectionId: string;
  enabled: boolean;
  serverId?: string;
}): Promise<void> {
  ensureCp();
  const { user, role } = await requireConnectionAdmin(input.orgId, input.connectionId);
  await cpSetPreviews(
    input.orgId,
    input.connectionId,
    { enabled: input.enabled, serverId: input.serverId },
    { name: user.name, role }
  );
  await writeAudit({
    orgId: input.orgId,
    actor: user.name,
    action: input.enabled ? "Previews enabled" : "Previews disabled",
    target: input.connectionId,
  });
  revalidatePath(`/dashboard/projects/${input.projectId}`);
}

/** List a connection's preview environments (open PRs first). */
export async function listPreviews(input: {
  orgId: string;
  connectionId: string;
}): Promise<CpPreviewEnvironment[]> {
  ensureCp();
  // P2-7 read scoping (SIGMA-93): resolve the connection's project and require
  // the caller can see it, so a scoped user can't read another project's preview
  // environments by connection id.
  const { connection } = await cpGetGitConnection(input.orgId, input.connectionId);
  await assertProjectVisible(input.orgId, connection.projectId);
  return cpListPreviews(input.orgId, input.connectionId);
}

/** GitHub App availability (SIGMA-55) — drives the Install/Link buttons. */
export async function getGitAppInfo(orgId: string): Promise<CpGitAppInfo> {
  ensureCp();
  await requireMembership(orgId);
  return cpGitAppInfo(orgId);
}

/** Link a GitHub App installation to a connection (the post-install callback
 *  lands here). Project Admin+ — it changes how the repo is authenticated. */
export async function linkInstallation(input: {
  orgId: string;
  projectId: string;
  connectionId: string;
  installationId: string;
}): Promise<void> {
  ensureCp();
  const { user, role } = await requireConnectionAdmin(input.orgId, input.connectionId);
  await cpLinkInstallation(input.orgId, input.connectionId, input.installationId, {
    name: user.name,
    role,
  });
  await writeAudit({
    orgId: input.orgId,
    actor: user.name,
    action: "Linked GitHub App installation",
    target: input.connectionId,
  });
  revalidatePath(`/dashboard/projects/${input.projectId}`);
}

// ── GitHub as an org-level integration ──────────────────────────────────────

/** The org's GitHub integration state, for the settings page and the repo
 *  picker. Read-only, so any member may load it. */
export async function getGitIntegration(orgId: string): Promise<CpGitIntegration> {
  ensureCp();
  await requireMembership(orgId);
  return cpGetGitIntegration(orgId);
}

/** Claim a GitHub App installation for the org — the post-install callback.
 *  Org-level, so it needs an Org Admin rather than a project role. */
export async function connectGitIntegration(input: {
  orgId: string;
  installationId: string;
}): Promise<CpGitHubInstallation> {
  ensureCp();
  const user = await requireOrgAdmin(input.orgId);
  const inst = await cpConnectGitIntegration(input.orgId, input.installationId, {
    name: user.name,
    role: "Org Admin",
  });
  await writeAudit({
    orgId: input.orgId,
    actor: user.name,
    action: "Connected GitHub integration",
    target: inst.accountLogin || inst.installationId,
  });
  revalidatePath("/dashboard/settings");
  return inst;
}

/** Disconnect an installation. Refuses while repos still deploy through it
 *  unless force is set — the caller shows what would break first. */
export async function disconnectGitIntegration(input: {
  orgId: string;
  installationId: string;
  force?: boolean;
}): Promise<void> {
  ensureCp();
  const user = await requireOrgAdmin(input.orgId);
  await cpDisconnectGitIntegration(
    input.orgId,
    input.installationId,
    { name: user.name, role: "Org Admin" },
    input.force ?? false
  );
  await writeAudit({
    orgId: input.orgId,
    actor: user.name,
    action: "Disconnected GitHub integration",
    target: input.installationId,
  });
  revalidatePath("/dashboard/settings");
}

/** Repos the org's installations can reach — the picker's options.
 *
 *  A CP/GitHub failure returns an empty, explicitly-not-connected list rather
 *  than throwing: Next.js redacts thrown server-action messages in production,
 *  so a throw would surface as an opaque digest instead of "GitHub isn't
 *  connected yet". */
export async function listGitRepos(orgId: string): Promise<CpRepoList> {
  ensureCp();
  await requireMembership(orgId);
  try {
    return await cpListGitRepos(orgId);
  } catch {
    return { repos: [], connected: false };
  }
}

/** Bind a repo to a project, deriving the git connection when needed. */
export async function selectGitRepo(input: {
  orgId: string;
  projectId: string;
  repoFullName: string;
  installationId?: string;
}): Promise<CpGitConnection> {
  ensureCp();
  await assertProjectVisible(input.orgId, input.projectId);
  const { user, role } = await requireProjectRole(input.orgId, input.projectId, "Project Admin");
  const conn = await cpSelectGitRepo(
    input.orgId,
    {
      projectId: input.projectId,
      repoFullName: input.repoFullName,
      installationId: input.installationId,
    },
    { name: user.name, role }
  );
  await writeAudit({
    orgId: input.orgId,
    actor: user.name,
    action: "Selected repository",
    target: conn.repoFullName,
  });
  revalidatePath(`/dashboard/projects/${input.projectId}`);
  return conn;
}
