"use server";

import { revalidatePath } from "next/cache";
import { requireMembership, requireProjectRole, assertProjectVisible } from "../active-org";
import { writeAudit } from "../audit";
import {
  cpEnabled,
  cpDetectRepo,
  cpConnectRepo,
  cpSetBranchMap,
  cpDeleteBranchMap,
  cpPromoteBranch,
  cpDisconnectRepo,
  cpSetPreviews,
  cpListPreviews,
  cpGetGitConnection,
  cpGitAppInfo,
  cpLinkInstallation,
  type CpGitAppInfo,
  type CpDetected,
  type CpGitConnection,
  type CpBranchMap,
  type CpPreviewEnvironment,
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
 *  can run it to see whether a repo is deployable before connecting. */
export async function detectRepo(input: {
  orgId: string;
  repoFullName: string;
  installationId?: string;
  token?: string;
}): Promise<CpDetected> {
  ensureCp();
  const { user, role } = await requireMembership(input.orgId);
  return cpDetectRepo(
    input.orgId,
    { repoFullName: input.repoFullName.trim(), installationId: input.installationId, token: input.token },
    { name: user.name, role }
  );
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

/** Map a branch to an environment with a deploy policy (auto | manual). */
export async function setBranchMapping(input: {
  orgId: string;
  projectId: string;
  connectionId: string;
  branch: string;
  environmentId: string;
  policy: "auto" | "manual";
}): Promise<CpBranchMap> {
  ensureCp();
  // Authorize on the connection's real project (SIGMA-93), not the client's.
  const { user, role } = await requireConnectionAdmin(input.orgId, input.connectionId);
  const branch = input.branch.trim();
  if (!branch) throw new Error("Branch is required.");
  const m = await cpSetBranchMap(
    input.orgId,
    input.connectionId,
    { branch, environmentId: input.environmentId, policy: input.policy },
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
