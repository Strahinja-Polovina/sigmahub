"use server";

import { revalidatePath } from "next/cache";
import { and, eq, ne } from "drizzle-orm";
import { db } from "../db";
import * as s from "../db/schema";
import { requireMembership } from "../active-org";
import { getProject, getResource } from "../queries";

function rid(prefix: string) {
  return `${prefix}_${crypto.randomUUID().replace(/-/g, "").slice(0, 12)}`;
}
function sha7() {
  return crypto.randomUUID().replace(/-/g, "").slice(0, 7);
}

async function assertResourceMembership(resourceId: string) {
  const resource = await getResource(resourceId);
  if (!resource) throw new Error("Resource not found.");
  const project = await getProject(resource.projectId);
  if (project) await requireMembership(project.orgId);
  return resource;
}

/** Deploy-from-Git result: create a resource and its first (running) deployment. */
export async function createResource(input: {
  projectId: string;
  environmentId: string;
  serverId?: string | null;
  name: string;
  kind: string;
  repo?: string;
  domain?: string;
}) {
  const project = await getProject(input.projectId);
  if (!project) throw new Error("Project not found.");
  await requireMembership(project.orgId);
  const name = input.name.trim();
  if (!name) throw new Error("Resource name is required.");

  // Don't trust the client-supplied env/server ids: they must belong to this
  // project/org, or a member of one org could plant a resource on another's
  // environment/server (IDOR).
  const [env] = await db
    .select({ projectId: s.environments.projectId })
    .from(s.environments)
    .where(eq(s.environments.id, input.environmentId));
  if (!env || env.projectId !== input.projectId) {
    throw new Error("Environment does not belong to this project.");
  }
  if (input.serverId) {
    const [sv] = await db
      .select({ orgId: s.servers.orgId })
      .from(s.servers)
      .where(eq(s.servers.id, input.serverId));
    if (!sv || sv.orgId !== project.orgId) {
      throw new Error("Server does not belong to this organization.");
    }
  }

  const id = rid("res");
  const now = new Date();
  await db.insert(s.resources).values({
    id,
    projectId: input.projectId,
    environmentId: input.environmentId,
    serverId: input.serverId ?? null,
    name,
    kind: input.kind,
    status: "running",
    repo: input.repo ?? null,
    domain: input.domain ?? null,
    version: "v1",
    lastDeployAt: now,
  });
  await db.insert(s.deployments).values({
    id: rid("dep"),
    resourceId: id,
    sha: sha7(),
    status: "running",
    author: "you",
    durationSec: 42,
    startedAt: now,
  });
  revalidatePath("/dashboard", "layout");
  return { id };
}

/** Kick off a redeploy: a new deployment enters the pipeline as `queued`. */
export async function deployResource(input: { resourceId: string }) {
  await assertResourceMembership(input.resourceId);
  const id = rid("dep");
  const now = new Date();
  await db.insert(s.deployments).values({
    id,
    resourceId: input.resourceId,
    sha: sha7(),
    status: "queued",
    author: "you",
    durationSec: 0,
    startedAt: now,
  });
  await db
    .update(s.resources)
    .set({ lastDeployAt: now })
    .where(eq(s.resources.id, input.resourceId));
  revalidatePath("/dashboard", "layout");
  revalidatePath(`/dashboard/resources/${input.resourceId}`);
  return { deploymentId: id };
}

/** Advance one deployment one step: queued → building → running. On reaching
 *  running it becomes the live build (older running builds → success). */
export async function advanceDeployment(input: { deploymentId: string }) {
  const [dep] = await db
    .select()
    .from(s.deployments)
    .where(eq(s.deployments.id, input.deploymentId));
  if (!dep) throw new Error("Deployment not found.");
  await assertResourceMembership(dep.resourceId);

  let next: string;
  if (dep.status === "queued") next = "building";
  else if (dep.status === "building") next = "running";
  else return { status: dep.status };

  if (next === "running") {
    await db
      .update(s.deployments)
      .set({ status: "success" })
      .where(
        and(
          eq(s.deployments.resourceId, dep.resourceId),
          eq(s.deployments.status, "running"),
          ne(s.deployments.id, dep.id)
        )
      );
    await db
      .update(s.resources)
      .set({ status: "running" })
      .where(eq(s.resources.id, dep.resourceId));
  }
  await db
    .update(s.deployments)
    .set({ status: next, durationSec: next === "running" ? 47 : 12 })
    .where(eq(s.deployments.id, input.deploymentId));

  revalidatePath("/dashboard", "layout");
  revalidatePath(`/dashboard/resources/${dep.resourceId}`);
  return { status: next };
}

export async function deleteResource(input: { resourceId: string }) {
  const resource = await getResource(input.resourceId);
  if (!resource) throw new Error("Resource not found.");
  const project = await getProject(resource.projectId);
  if (!project) throw new Error("Parent project not found.");
  await requireMembership(project.orgId);
  await db.delete(s.resources).where(eq(s.resources.id, input.resourceId));
  revalidatePath("/dashboard", "layout");
}
