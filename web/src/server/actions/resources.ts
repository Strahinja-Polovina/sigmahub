"use server";

import { revalidatePath } from "next/cache";
import { and, eq, ne } from "drizzle-orm";
import { db } from "../db";
import * as s from "../db/schema";
import { requireMembership, requireProjectAdmin } from "../active-org";
import { getProject, getResource } from "../queries";
import { writeAudit } from "../audit";
import { cpEnabled, cpCreateResource, cpDeleteResource, cpKind, cpMirrorServer } from "../cp";

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
  const membership = project ? await requireMembership(project.orgId) : null;
  return { resource, orgId: project?.orgId ?? null, actor: membership?.user ?? null };
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
  const { user, role } = await requireProjectAdmin(project.orgId);
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
  // Demo mode resolves the server from the local table; in CP mode servers
  // live in the control plane, so the ownership check + FK-satisfying local
  // mirror happen below (cpMirrorServer 404s cross-org ids).
  if (input.serverId && !cpEnabled()) {
    const [sv] = await db
      .select({ orgId: s.servers.orgId })
      .from(s.servers)
      .where(eq(s.servers.id, input.serverId));
    if (!sv || sv.orgId !== project.orgId) {
      throw new Error("Server does not belong to this organization.");
    }
  }

  let id = rid("res");
  if (cpEnabled()) {
    // CP mode: the control plane owns the resource record and enforces the
    // kind/server-type availability matrix + env attachment server-side. The
    // local row mirrors it under the same id so the (still simulated, P1-9)
    // deploy timeline keeps rendering; mirror the CP server first so the local
    // resources.server_id FK holds.
    if (!input.serverId) throw new Error("A target server is required.");
    await cpMirrorServer(project.orgId, input.serverId);
    const created = await cpCreateResource(
      project.orgId,
      {
        environmentId: input.environmentId,
        serverId: input.serverId,
        name,
        kind: cpKind(input.kind),
        spec: { repo: input.repo ?? null, domain: input.domain ?? null },
      },
      { name: user.name, role }
    );
    id = created.id;
  }
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
  await writeAudit({ orgId: project.orgId, actor: user.name, action: "Deployed resource", target: `${name} · ${input.kind}` });
  revalidatePath("/dashboard", "layout");
  return { id };
}

/** Kick off a redeploy: a new deployment enters the pipeline as `queued`. */
export async function deployResource(input: { resourceId: string }) {
  const { resource, orgId, actor } = await assertResourceMembership(input.resourceId);
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
  // Audit only after the redeploy is actually enqueued, so a failed insert
  // can't leave a phantom "Redeployed" row (matches every sibling action).
  if (orgId && actor) {
    await writeAudit({ orgId, actor: actor.name, action: "Redeployed resource", target: resource.name });
  }
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
  if (!dep) return { status: null };
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
  if (!resource) return;
  const project = await getProject(resource.projectId);
  const membership = project ? await requireProjectAdmin(project.orgId) : null;
  if (project && membership && cpEnabled()) {
    await cpDeleteResource(project.orgId, input.resourceId, {
      name: membership.user.name,
      role: membership.role,
    });
  }
  await db.delete(s.resources).where(eq(s.resources.id, input.resourceId));
  if (project && membership) {
    await writeAudit({ orgId: project.orgId, actor: membership.user.name, action: "Deleted resource", target: resource.name });
  }
  revalidatePath("/dashboard", "layout");
}
