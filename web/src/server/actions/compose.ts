"use server";

import { revalidatePath } from "next/cache";
import { requireProjectAdminForResource, requireResourceVisible } from "../active-org";
import { writeAudit } from "../audit";
import {
  cpEnabled,
  cpGetComposeServices,
  cpSetComposePlacements,
  type CpComposeServices,
} from "../cp";

function ensureCp() {
  if (!cpEnabled()) {
    throw new Error("Compose placement requires the control plane (set SIGMAHUB_CP_URL).");
  }
}

/** The app's Compose service graph with its current placement. */
export async function getComposeServices(input: {
  orgId: string;
  resourceId: string;
}): Promise<CpComposeServices | null> {
  ensureCp();
  await requireResourceVisible(input.orgId, input.resourceId);
  try {
    return await cpGetComposeServices(input.orgId, input.resourceId);
  } catch {
    // Not a Compose app (or the CP is unreachable) — the caller hides the panel
    // rather than showing a broken one.
    return null;
  }
}

/** Move services between servers and set their per-service environment. */
export async function setComposePlacements(input: {
  orgId: string;
  resourceId: string;
  placements: { service: string; serverId: string; env?: Record<string, string> }[];
}): Promise<{ servers: string[] }> {
  ensureCp();
  const { user, role } = await requireProjectAdminForResource(input.orgId, input.resourceId);
  const res = await cpSetComposePlacements(
    input.orgId,
    input.resourceId,
    input.placements,
    { name: user.name, role }
  );
  await writeAudit({
    orgId: input.orgId,
    actor: user.name,
    action: "Updated compose placement",
    target: input.placements.map((p) => p.service).join(", "),
  });
  revalidatePath(`/dashboard/resources/${input.resourceId}`);
  return { servers: res.servers };
}
