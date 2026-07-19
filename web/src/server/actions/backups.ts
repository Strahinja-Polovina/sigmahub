"use server";

import { revalidatePath } from "next/cache";
import { and, eq } from "drizzle-orm";
import { db } from "../db";
import * as s from "../db/schema";
import {
  requireMembership,
  requireProjectAdmin,
  requireProjectAdminForResource,
  requireResourceVisible,
} from "../active-org";
import { writeAudit } from "../audit";
import {
  cpEnabled,
  cpListBackupTargets,
  cpCreateBackupTarget,
  cpDeleteBackupTarget,
  cpUpdateBackupPolicy,
  cpListBackupRuns,
  cpRestoreDatabase,
  cpRestoreDatabaseToTimestamp,
  type CpBackupTarget,
  type CpBackupRun,
  type CpBackupPolicy,
} from "../cp";

/** Backups execute on the control plane (P1-11); the PGlite demo has none. */
function ensureCp() {
  if (!cpEnabled()) {
    throw new Error("Backups require the control plane (set SIGMAHUB_CP_URL).");
  }
}

/** Defense-in-depth (SIGMA-76): don't forward client-supplied restore-target
 *  ids to the CP unchecked. The target environment must live in the SOURCE
 *  resource's project, and the target server must belong to the org — mirroring
 *  the createResource IDOR guards. The CP re-validates under the org token, but
 *  the web fails closed independently so a malformed/hostile target is rejected
 *  before it ever reaches the control plane. */
async function assertRestoreTarget(
  orgId: string,
  resourceId: string,
  environmentId: string,
  serverId: string
) {
  // Resolve the source resource's project inside THIS org (org-scoped join).
  const [res] = await db
    .select({ projectId: s.resources.projectId })
    .from(s.resources)
    .innerJoin(s.projects, eq(s.resources.projectId, s.projects.id))
    .where(and(eq(s.resources.id, resourceId), eq(s.projects.orgId, orgId)));
  if (res) {
    // Environments are web-owned in both modes, so this is authoritative.
    const [env] = await db
      .select({ projectId: s.environments.projectId })
      .from(s.environments)
      .where(eq(s.environments.id, environmentId));
    if (!env || env.projectId !== res.projectId) {
      throw new Error("Target environment does not belong to this resource's project.");
    }
  }
  // Servers live in the CP in CP mode; the local table is a mirror that may
  // lag. Reject an id the mirror knows belongs to another org; defer an unknown
  // id to the CP (which 404s cross-org ids under the org token).
  const [sv] = await db
    .select({ orgId: s.servers.orgId })
    .from(s.servers)
    .where(eq(s.servers.id, serverId));
  if (sv && sv.orgId !== orgId) {
    throw new Error("Target server does not belong to this organization.");
  }
}

export async function listBackupTargets(input: { orgId: string }): Promise<CpBackupTarget[]> {
  ensureCp();
  await requireMembership(input.orgId);
  return cpListBackupTargets(input.orgId);
}

export async function createBackupTarget(input: {
  orgId: string;
  name: string;
  endpoint: string;
  bucket: string;
  region: string;
  accessKey: string;
  secretKey: string;
}): Promise<CpBackupTarget> {
  ensureCp();
  const { user, role } = await requireProjectAdmin(input.orgId);
  const t = await cpCreateBackupTarget(
    input.orgId,
    {
      name: input.name.trim(),
      endpoint: input.endpoint.trim(),
      bucket: input.bucket.trim(),
      region: input.region.trim(),
      accessKey: input.accessKey.trim(),
      secretKey: input.secretKey,
    },
    { name: user.name, role }
  );
  await writeAudit({ orgId: input.orgId, actor: user.name, action: "Backup target created", target: t.name });
  return t;
}

export async function deleteBackupTarget(input: { orgId: string; targetId: string; name: string }) {
  ensureCp();
  const { user, role } = await requireProjectAdmin(input.orgId);
  await cpDeleteBackupTarget(input.orgId, input.targetId, { name: user.name, role });
  await writeAudit({ orgId: input.orgId, actor: user.name, action: "Backup target deleted", target: input.name });
}

export async function setBackupPolicy(input: {
  orgId: string;
  resourceId: string;
  targetId?: string | null;
  enabled?: boolean;
  pitrEnabled?: boolean;
}): Promise<CpBackupPolicy> {
  ensureCp();
  const { user, role } = await requireProjectAdminForResource(input.orgId, input.resourceId);
  const bp = await cpUpdateBackupPolicy(
    input.orgId,
    input.resourceId,
    { targetId: input.targetId, enabled: input.enabled, pitrEnabled: input.pitrEnabled },
    { name: user.name, role }
  );
  await writeAudit({ orgId: input.orgId, actor: user.name, action: "Backup policy updated", target: input.resourceId });
  revalidatePath(`/dashboard/resources/${input.resourceId}`);
  return bp;
}

export async function listBackupRuns(input: { orgId: string; resourceId: string }): Promise<CpBackupRun[]> {
  ensureCp();
  // P2-7: backup/restore history is per-resource; gate on project visibility
  // (SIGMA-84), not bare org membership.
  await requireResourceVisible(input.orgId, input.resourceId);
  return cpListBackupRuns(input.orgId, input.resourceId);
}

/** Fire-drill restore into a FRESH database resource on the same environment/
 *  server as the source (the common drill), named by the caller. */
export async function restoreDatabase(input: {
  orgId: string;
  resourceId: string;
  name: string;
  environmentId: string;
  serverId: string;
}) {
  ensureCp();
  const { user, role } = await requireProjectAdminForResource(input.orgId, input.resourceId);
  await assertRestoreTarget(input.orgId, input.resourceId, input.environmentId, input.serverId);
  const out = await cpRestoreDatabase(
    input.orgId,
    input.resourceId,
    { name: input.name.trim(), environmentId: input.environmentId, serverId: input.serverId },
    { name: user.name, role }
  );
  await writeAudit({
    orgId: input.orgId,
    actor: user.name,
    action: "Restore queued",
    target: `${input.resourceId} -> ${out.resource.id}`,
  });
  revalidatePath("/dashboard", "layout");
  return out;
}

/** P2-5b: point-in-time recovery — provision a fresh postgres resource
 *  recovered to targetTime. The CP validates the recoverable window. */
export async function restoreDatabaseToTimestamp(input: {
  orgId: string;
  resourceId: string;
  name: string;
  environmentId: string;
  serverId: string;
  targetTime: string; // RFC3339
}) {
  ensureCp();
  const { user, role } = await requireProjectAdminForResource(input.orgId, input.resourceId);
  await assertRestoreTarget(input.orgId, input.resourceId, input.environmentId, input.serverId);
  const out = await cpRestoreDatabaseToTimestamp(
    input.orgId,
    input.resourceId,
    {
      name: input.name.trim(),
      environmentId: input.environmentId,
      serverId: input.serverId,
      targetTime: input.targetTime,
    },
    { name: user.name, role }
  );
  await writeAudit({
    orgId: input.orgId,
    actor: user.name,
    action: "PITR restore queued",
    target: `${input.resourceId} -> ${out.resource.id} @ ${input.targetTime}`,
  });
  revalidatePath("/dashboard", "layout");
  return out;
}
