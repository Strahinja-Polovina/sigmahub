"use server";

import { requireMembership, requireProjectAdminForResource } from "../active-org";
import { writeAudit } from "../audit";
import {
  cpEnabled,
  cpGetDatabase,
  cpRevealDatabaseConnection,
  type CpDatabaseInfo,
  type CpDatabaseConnection,
} from "../cp";

/** Real database provisioning lives in the control plane (P1-10); the PGlite
 *  demo keeps its simulated resources and never reaches these actions. */
function ensureCp() {
  if (!cpEnabled()) {
    throw new Error("Database resources require the control plane (set SIGMAHUB_CP_URL).");
  }
}

/** Non-secret connection metadata + backup policy. Any org member. */
export async function getDatabaseInfo(input: {
  orgId: string;
  resourceId: string;
}): Promise<CpDatabaseInfo | null> {
  ensureCp();
  await requireMembership(input.orgId);
  return cpGetDatabase(input.orgId, input.resourceId);
}

/** Audited credential reveal. Project Admin+ — a Developer is rejected here
 *  AND by the control plane route (defense in depth), and every successful
 *  reveal writes an audit row on both sides. */
export async function revealDatabaseConnection(input: {
  orgId: string;
  resourceId: string;
}): Promise<CpDatabaseConnection> {
  ensureCp();
  const { user, role } = await requireProjectAdminForResource(input.orgId, input.resourceId);
  const conn = await cpRevealDatabaseConnection(input.orgId, input.resourceId, {
    name: user.name,
    role,
  });
  await writeAudit({
    orgId: input.orgId,
    actor: user.name,
    action: "DB credentials revealed",
    target: input.resourceId,
  });
  return conn;
}
