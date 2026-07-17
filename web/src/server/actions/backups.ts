"use server";

import { revalidatePath } from "next/cache";
import { requireMembership, requireProjectAdmin } from "../active-org";
import { writeAudit } from "../audit";
import {
  cpEnabled,
  cpListBackupTargets,
  cpCreateBackupTarget,
  cpDeleteBackupTarget,
  cpUpdateBackupPolicy,
  cpListBackupRuns,
  cpRestoreDatabase,
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
}): Promise<CpBackupPolicy> {
  ensureCp();
  const { user, role } = await requireProjectAdmin(input.orgId);
  const bp = await cpUpdateBackupPolicy(
    input.orgId,
    input.resourceId,
    { targetId: input.targetId, enabled: input.enabled },
    { name: user.name, role }
  );
  await writeAudit({ orgId: input.orgId, actor: user.name, action: "Backup policy updated", target: input.resourceId });
  revalidatePath(`/dashboard/resources/${input.resourceId}`);
  return bp;
}

export async function listBackupRuns(input: { orgId: string; resourceId: string }): Promise<CpBackupRun[]> {
  ensureCp();
  await requireMembership(input.orgId);
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
  const { user, role } = await requireProjectAdmin(input.orgId);
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
