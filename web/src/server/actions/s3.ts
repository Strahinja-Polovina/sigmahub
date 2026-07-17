"use server";

// S3 storage (P2-1): the info/reveal split mirrors databases — metadata for
// every member, the audited credential reveal for Project Admin+.

import { requireMembership, requireProjectAdminForResource } from "../active-org";
import { writeAudit } from "../audit";
import {
  cpEnabled,
  cpGetS3,
  cpRevealS3Connection,
  type CpS3Connection,
  type CpS3Info,
} from "../cp";

function ensureCp() {
  if (!cpEnabled()) {
    throw new Error("S3 storage requires the control plane (set SIGMAHUB_CP_URL).");
  }
}

export async function getS3Info(input: {
  orgId: string;
  resourceId: string;
}): Promise<CpS3Info | null> {
  ensureCp();
  await requireMembership(input.orgId);
  return cpGetS3(input.orgId, input.resourceId);
}

export async function revealS3Connection(input: {
  orgId: string;
  resourceId: string;
}): Promise<CpS3Connection> {
  ensureCp();
  const { user, role } = await requireProjectAdminForResource(input.orgId, input.resourceId);
  const conn = await cpRevealS3Connection(input.orgId, input.resourceId, {
    name: user.name,
    role,
  });
  await writeAudit({
    orgId: input.orgId,
    actor: user.name,
    action: "S3 credentials revealed",
    target: input.resourceId,
  });
  return conn;
}
