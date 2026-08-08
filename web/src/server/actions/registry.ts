"use server";

import { revalidatePath } from "next/cache";
import { requireOrgAdmin, requireMembership } from "../active-org";
import { writeAudit } from "../audit";
import {
  cpEnabled,
  cpGetRegistry,
  cpSetRegistry,
  cpDeleteRegistry,
  type CpImageRegistry,
} from "../cp";

/** A registry is only meaningful with a control plane rendering image tags. */
function ensureCp() {
  if (!cpEnabled()) {
    throw new Error("A container registry requires the control plane (set SIGMAHUB_CP_URL).");
  }
}

/** The org's registry, or the not-configured state.
 *
 *  A CP failure degrades to "not configured" rather than throwing: Next.js
 *  redacts thrown server-action messages in production, so a throw would reach
 *  the user as an opaque digest instead of a form they can fill in. */
export async function getRegistry(input: { orgId: string }): Promise<{
  configured: boolean;
  registry: CpImageRegistry | null;
  repository: string;
}> {
  if (!cpEnabled()) return { configured: false, registry: null, repository: "" };
  await requireMembership(input.orgId);
  try {
    const res = await cpGetRegistry(input.orgId);
    return {
      configured: res.configured,
      registry: res.registry ?? null,
      repository: res.repository ?? "",
    };
  } catch {
    return { configured: false, registry: null, repository: "" };
  }
}

/** Configure the registry. An empty password KEEPS the stored one, so editing
 *  the namespace does not silently clear the credential and break every push. */
export async function setRegistry(input: {
  orgId: string;
  host: string;
  namespace?: string;
  username?: string;
  password?: string;
}): Promise<{ repository: string }> {
  ensureCp();
  const user = await requireOrgAdmin(input.orgId);
  const res = await cpSetRegistry(
    input.orgId,
    {
      host: input.host,
      namespace: input.namespace,
      username: input.username,
      password: input.password,
    },
    { name: user.name, role: "Org Admin" }
  );
  await writeAudit({
    orgId: input.orgId,
    actor: user.name,
    action: "Configured container registry",
    target: res.repository,
  });
  revalidatePath("/dashboard/settings");
  return { repository: res.repository };
}

/** Remove the registry. Deploys that need one start failing with a clear
 *  message rather than pushing somewhere the org does not own. */
export async function removeRegistry(input: { orgId: string }): Promise<void> {
  ensureCp();
  const user = await requireOrgAdmin(input.orgId);
  await cpDeleteRegistry(input.orgId, { name: user.name, role: "Org Admin" });
  await writeAudit({
    orgId: input.orgId,
    actor: user.name,
    action: "Removed container registry",
    target: "",
  });
  revalidatePath("/dashboard/settings");
}
