"use server";

import { revalidatePath } from "next/cache";
import { requireProjectAdminForResource } from "../active-org";
import { writeAudit } from "../audit";
import { cpEnabled, cpAttachDomain, cpDetachDomain, type CpDomain } from "../cp";

/** Custom domains are a control-plane feature (Traefik + ACME live there). */
function ensureCp() {
  if (!cpEnabled()) {
    throw new Error("Custom domains require the control plane (set SIGMAHUB_CP_URL).");
  }
}

/** Attach a custom domain to an app resource. Project Admin+. */
export async function attachDomain(input: {
  orgId: string;
  resourceId: string;
  domain: string;
  challengeType?: string;
}): Promise<CpDomain> {
  ensureCp();
  const { user, role } = await requireProjectAdminForResource(input.orgId, input.resourceId);
  const domain = input.domain.trim().toLowerCase();
  if (!domain.includes(".")) throw new Error("Enter a valid domain (e.g. app.example.com).");
  const d = await cpAttachDomain(
    input.orgId,
    input.resourceId,
    { domain, challengeType: input.challengeType },
    { name: user.name, role }
  );
  await writeAudit({ orgId: input.orgId, actor: user.name, action: `Attached domain ${domain}`, target: input.resourceId });
  revalidatePath(`/dashboard/resources/${input.resourceId}`);
  return d;
}

export async function detachDomain(input: {
  orgId: string;
  resourceId: string;
  domainId: string;
  domain: string;
}): Promise<void> {
  ensureCp();
  const { user, role } = await requireProjectAdminForResource(input.orgId, input.resourceId);
  await cpDetachDomain(input.orgId, input.domainId, { name: user.name, role });
  await writeAudit({ orgId: input.orgId, actor: user.name, action: `Detached domain ${input.domain}`, target: input.resourceId });
  revalidatePath(`/dashboard/resources/${input.resourceId}`);
}
