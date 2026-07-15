"use server";

import { revalidatePath } from "next/cache";
import { requireMembership, requireProjectAdmin } from "../active-org";
import { getProject, getResource } from "../queries";
import { writeAudit } from "../audit";
import { putSecret, readSecretValue, removeSecret, secretName } from "../secrets-data";

/** Resolve a resource to its org/project/environment, asserting the caller is a
 *  member of the owning org. Returns the membership so callers can escalate the
 *  role check (reveal/create/delete need Project Admin+). */
async function resourceScope(resourceId: string) {
  const resource = await getResource(resourceId);
  if (!resource) throw new Error("Resource not found.");
  const project = await getProject(resource.projectId);
  if (!project) throw new Error("Resource not found.");
  // Membership check first so a non-member can't probe scope existence.
  await requireMembership(project.orgId);
  return {
    orgId: project.orgId,
    projectId: resource.projectId,
    environmentId: resource.environmentId,
    resourceName: resource.name,
  };
}

export async function createSecretAction(input: {
  resourceId: string;
  name: string;
  value: string;
  scope: "project" | "environment";
  envVar: boolean;
}) {
  const { orgId, projectId, environmentId, resourceName } = await resourceScope(input.resourceId);
  // Create is Project Admin+ (matches the CP route); a Developer is refused here
  // before any value reaches the control plane.
  const { user, role } = await requireProjectAdmin(orgId);

  const name = input.name.trim();
  if (!name) throw new Error("Secret name is required.");
  if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(name)) {
    throw new Error("Name must be a valid identifier (letters, digits, underscore; no leading digit).");
  }

  await putSecret(
    orgId,
    projectId,
    {
      name,
      value: input.value,
      environmentId: input.scope === "environment" ? environmentId : "",
      envVar: input.envVar,
    },
    { name: user.name, role }
  );
  await writeAudit({
    orgId,
    actor: user.name,
    action: "Secret created",
    target: `${resourceName} · ${name} (${input.scope})`,
  });
  revalidatePath(`/dashboard/resources/${input.resourceId}`);
}

export async function revealSecretAction(input: { resourceId: string; secretId: string }) {
  const { orgId } = await resourceScope(input.resourceId);
  // Reveal is Project Admin+; a Developer 403s here (and again at the CP, which
  // caps the effective role by the forwarded actor and audits the read).
  const { user, role } = await requireProjectAdmin(orgId);
  const value = await readSecretValue(orgId, input.secretId, { name: user.name, role });
  // In demo mode the CP isn't the one auditing, so record the read locally.
  const name = await secretName(orgId, input.secretId);
  await writeAudit({
    orgId,
    actor: user.name,
    action: "Secret revealed",
    target: name ?? input.secretId,
  });
  return { value };
}

export async function deleteSecretAction(input: { resourceId: string; secretId: string }) {
  const { orgId, resourceName } = await resourceScope(input.resourceId);
  const { user, role } = await requireProjectAdmin(orgId);
  const name = await secretName(orgId, input.secretId);
  await removeSecret(orgId, input.secretId, { name: user.name, role });
  await writeAudit({
    orgId,
    actor: user.name,
    action: "Secret deleted",
    target: name ? `${resourceName} · ${name}` : resourceName,
  });
  revalidatePath(`/dashboard/resources/${input.resourceId}`);
}
