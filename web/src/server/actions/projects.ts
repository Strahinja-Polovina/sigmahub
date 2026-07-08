"use server";

import { revalidatePath } from "next/cache";
import { and, eq } from "drizzle-orm";
import { db } from "../db";
import * as s from "../db/schema";
import { requireMembership } from "../active-org";
import { getProject } from "../queries";
import { writeAudit } from "../audit";

function slugify(x: string) {
  return (
    x
      .trim()
      .toLowerCase()
      .replace(/[^a-z0-9]+/g, "-")
      .replace(/^-+|-+$/g, "")
      .slice(0, 40) || "project"
  );
}

function rid(prefix: string) {
  return `${prefix}_${crypto.randomUUID().replace(/-/g, "").slice(0, 12)}`;
}

export async function createProject(input: {
  orgId: string;
  name: string;
  description?: string;
}) {
  const { user } = await requireMembership(input.orgId);
  const name = input.name.trim();
  if (!name) throw new Error("Project name is required.");
  const id = rid("proj");
  await db.insert(s.projects).values({
    id,
    orgId: input.orgId,
    name,
    slug: slugify(name),
    description: input.description?.trim() ?? "",
  });
  await writeAudit({ orgId: input.orgId, actor: user.name, action: "Created project", target: name });
  revalidatePath("/dashboard", "layout");
  return { id };
}

export async function renameProject(input: {
  projectId: string;
  name: string;
  description?: string;
}) {
  const project = await getProject(input.projectId);
  if (!project) throw new Error("Project not found.");
  const { user } = await requireMembership(project.orgId);
  const name = input.name.trim();
  if (!name) throw new Error("Project name is required.");
  await db
    .update(s.projects)
    .set({
      name,
      slug: slugify(name),
      description: input.description?.trim() ?? project.description,
    })
    .where(eq(s.projects.id, input.projectId));
  await writeAudit({
    orgId: project.orgId,
    actor: user.name,
    action: "Renamed project",
    target: project.name === name ? name : `${project.name} → ${name}`,
  });
  revalidatePath("/dashboard", "layout");
}

export async function deleteProject(input: { projectId: string }) {
  const project = await getProject(input.projectId);
  if (!project) return;
  const { user } = await requireMembership(project.orgId);
  // FK cascade removes environments, env_servers, resources and deployments.
  await db.delete(s.projects).where(eq(s.projects.id, input.projectId));
  await writeAudit({ orgId: project.orgId, actor: user.name, action: "Deleted project", target: project.name });
  revalidatePath("/dashboard", "layout");
}

export async function createEnvironment(input: {
  projectId: string;
  name: string;
}) {
  const project = await getProject(input.projectId);
  if (!project) throw new Error("Project not found.");
  const { user } = await requireMembership(project.orgId);
  const name = input.name.trim();
  if (!name) throw new Error("Environment name is required.");
  const id = rid("env");
  await db.insert(s.environments).values({ id, projectId: input.projectId, name });
  await writeAudit({ orgId: project.orgId, actor: user.name, action: "Created environment", target: `${project.name} / ${name}` });
  revalidatePath("/dashboard", "layout");
  return { id };
}

/** Resolve an environment to its project + assert the caller's membership.
 *  Shared by the attach/detach actions below. */
async function requireEnvMembership(environmentId: string) {
  const [env] = await db
    .select()
    .from(s.environments)
    .where(eq(s.environments.id, environmentId));
  if (!env) throw new Error("Environment not found.");
  const project = await getProject(env.projectId);
  if (!project) throw new Error("Project not found.");
  const { user } = await requireMembership(project.orgId);
  return { env, project, user };
}

/** Attach an org server to an environment so deploys can target it. */
export async function attachServerToEnv(input: {
  environmentId: string;
  serverId: string;
}) {
  const { env, project, user } = await requireEnvMembership(input.environmentId);
  // The server must belong to the same org (don't trust the client id — IDOR).
  const [sv] = await db
    .select({ orgId: s.servers.orgId, name: s.servers.name })
    .from(s.servers)
    .where(eq(s.servers.id, input.serverId));
  if (!sv || sv.orgId !== project.orgId) {
    throw new Error("Server does not belong to this organization.");
  }
  await db
    .insert(s.envServers)
    .values({ environmentId: input.environmentId, serverId: input.serverId })
    .onConflictDoNothing();
  await writeAudit({
    orgId: project.orgId,
    actor: user.name,
    action: "Attached server",
    target: `${sv.name} → ${project.name} / ${env.name}`,
  });
  revalidatePath("/dashboard", "layout");
}

export async function detachServerFromEnv(input: {
  environmentId: string;
  serverId: string;
}) {
  const { env, project, user } = await requireEnvMembership(input.environmentId);
  // Same ownership rule as attach: never resolve another org's server (IDOR /
  // cross-org name disclosure via the audit log).
  const [sv] = await db
    .select({ orgId: s.servers.orgId, name: s.servers.name })
    .from(s.servers)
    .where(eq(s.servers.id, input.serverId));
  if (!sv || sv.orgId !== project.orgId) {
    throw new Error("Server does not belong to this organization.");
  }
  // Don't silently orphan workloads: resources in this environment still
  // running on the server keep their serverId, so block until they're gone.
  const hosted = await db
    .select({ id: s.resources.id })
    .from(s.resources)
    .where(
      and(
        eq(s.resources.environmentId, input.environmentId),
        eq(s.resources.serverId, input.serverId)
      )
    );
  if (hosted.length > 0) {
    throw new Error(
      `${hosted.length} resource${hosted.length === 1 ? "" : "s"} in this environment still run${hosted.length === 1 ? "s" : ""} on this server. Delete or redeploy them first.`
    );
  }
  const deleted = await db
    .delete(s.envServers)
    .where(
      and(
        eq(s.envServers.environmentId, input.environmentId),
        eq(s.envServers.serverId, input.serverId)
      )
    )
    .returning({ serverId: s.envServers.serverId });
  // No row deleted → nothing happened; don't fabricate an audit event.
  if (deleted.length > 0) {
    await writeAudit({
      orgId: project.orgId,
      actor: user.name,
      action: "Detached server",
      target: `${sv.name} ← ${project.name} / ${env.name}`,
    });
  }
  revalidatePath("/dashboard", "layout");
}

export async function deleteEnvironment(input: { environmentId: string }) {
  const [env] = await db
    .select()
    .from(s.environments)
    .where(eq(s.environments.id, input.environmentId));
  if (!env) return;
  const project = await getProject(env.projectId);
  const membership = project ? await requireMembership(project.orgId) : null;
  await db.delete(s.environments).where(eq(s.environments.id, input.environmentId));
  if (project && membership) {
    await writeAudit({
      orgId: project.orgId,
      actor: membership.user.name,
      action: "Deleted environment",
      target: `${project.name} / ${env.name}`,
    });
  }
  revalidatePath("/dashboard", "layout");
}
