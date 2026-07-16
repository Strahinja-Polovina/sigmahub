"use server";

import { requireProjectAdmin } from "../active-org";
import { writeAudit } from "../audit";
import { cpEnabled, cpRevealDBConnection } from "../cp";

/** Reveal a database resource's connection string (mesh-internal only in v1).
 *  Project Admin+ — a Developer is rejected here AND by the CP's role gate;
 *  the reveal is audited on both sides. */
export async function revealDBConnection(input: {
  orgId: string;
  resourceId: string;
}): Promise<{ connectionString: string; scope: string }> {
  if (!cpEnabled()) {
    throw new Error("Database connections require the control plane (set SIGMAHUB_CP_URL).");
  }
  const { user, role } = await requireProjectAdmin(input.orgId);
  const res = await cpRevealDBConnection(input.orgId, input.resourceId, { name: user.name, role });
  await writeAudit({
    orgId: input.orgId,
    actor: user.name,
    action: "Revealed database connection",
    target: input.resourceId,
  });
  return res;
}
