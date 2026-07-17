"use server";

import { revalidatePath } from "next/cache";
import { requireMembership, requireProjectAdminForResource } from "../active-org";
import { writeAudit } from "../audit";
import {
  cpEnabled,
  cpCreateRollback,
  cpDeployLogs,
  type CpDeployment,
  type CpDeployLog,
} from "../cp";

/** Deployments are a control-plane feature (the reconciler renders the pipeline). */
function ensureCp() {
  if (!cpEnabled()) {
    throw new Error("Deployments require the control plane (set SIGMAHUB_CP_URL).");
  }
}

/** Roll a resource back to a prior successful release (rebuild-free). Project Admin+. */
export async function rollbackDeployment(input: {
  orgId: string;
  resourceId: string;
  targetDeploymentId: string;
}): Promise<CpDeployment> {
  ensureCp();
  const { user, role } = await requireProjectAdminForResource(input.orgId, input.resourceId);
  const dep = await cpCreateRollback(input.orgId, input.resourceId, input.targetDeploymentId, {
    name: user.name,
    role,
  });
  await writeAudit({
    orgId: input.orgId,
    actor: user.name,
    action: `Rolled back to ${input.targetDeploymentId}`,
    target: input.resourceId,
  });
  revalidatePath(`/dashboard/resources/${input.resourceId}`);
  return dep;
}

/** Poll a deployment's build/orchestration logs past a cursor. Member-visible;
 *  the client component calls this on an interval while a deploy is in-flight. */
export async function fetchDeployLogs(input: {
  orgId: string;
  deploymentId: string;
  after?: number;
}): Promise<{ deployment: CpDeployment; logs: CpDeployLog[]; nextCursor: number; done: boolean }> {
  ensureCp();
  await requireMembership(input.orgId);
  return cpDeployLogs(input.orgId, input.deploymentId, input.after ?? 0);
}
