"use server";

import { revalidatePath } from "next/cache";
import { and, eq } from "drizzle-orm";
import { db } from "../db";
import * as s from "../db/schema";
import { requireProjectAdmin, requireProjectRole } from "../active-org";
import { getProject } from "../queries";
import { writeAudit } from "../audit";
import {
  cpEnabled,
  cpCreateProject,
  cpUpdateProject,
  cpDeleteProject,
  cpCreateEnvironment,
  cpUpdateEnvironment,
  cpDeleteEnvironment,
  cpAttachServer,
  cpDetachServer,
  cpMirrorServer,
} from "../cp";

/** Free-text env names; "production"/"prod" carry the CP production flag. */
function isProductionName(name: string) {
  const n = name.trim().toLowerCase();
  return n === "production" || n === "prod";
}

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
  const { user, role } = await requireProjectAdmin(input.orgId);
  const name = input.name.trim();
  if (!name) throw new Error("Project name is required.");
  // CP mode: the control plane is the source of truth (P1-1); the local row
  // mirrors it under the SAME id so the read models keep working until later
  // tickets migrate reads to the CP.
  let id = rid("proj");
  if (cpEnabled()) {
    const created = await cpCreateProject(
      input.orgId,
      { name, description: input.description?.trim() },
      { name: user.name, role }
    );
    id = created.id;
  }
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
  const { user, role } = await requireProjectRole(project.orgId, project.id, "Project Admin");
  const name = input.name.trim();
  if (!name) throw new Error("Project name is required.");
  if (cpEnabled()) {
    await cpUpdateProject(
      project.orgId,
      input.projectId,
      { name, description: input.description?.trim() ?? project.description },
      { name: user.name, role }
    );
  }
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
  const { user, role } = await requireProjectRole(project.orgId, project.id, "Project Admin");
  if (cpEnabled()) {
    await cpDeleteProject(project.orgId, input.projectId, { name: user.name, role });
  }
  // FK cascade removes environments, env_servers, resources and deployments.
  await db.delete(s.projects).where(eq(s.projects.id, input.projectId));
  await writeAudit({ orgId: project.orgId, actor: user.name, action: "Deleted project", target: project.name });
  revalidatePath("/dashboard", "layout");
}

export async function createEnvironment(input: {
  projectId: string;
  name: string;
  /** Explicit production choice from the dialog (SIGMA-190). The name-based
   *  heuristic remains only as the dialog's DEFAULT — the user decides. */
  production?: boolean;
}) {
  const project = await getProject(input.projectId);
  if (!project) throw new Error("Project not found.");
  const { user, role } = await requireProjectRole(project.orgId, project.id, "Project Admin");
  const name = input.name.trim();
  if (!name) throw new Error("Environment name is required.");
  const production = input.production ?? isProductionName(name);
  let id = rid("env");
  if (cpEnabled()) {
    const created = await cpCreateEnvironment(
      project.orgId,
      input.projectId,
      { name, production },
      { name: user.name, role }
    );
    id = created.id;
  }
  await db.insert(s.environments).values({ id, projectId: input.projectId, name, production });
  await writeAudit({ orgId: project.orgId, actor: user.name, action: "Created environment", target: `${project.name} / ${name}` });
  revalidatePath("/dashboard", "layout");
  return { id };
}

/** Flip an environment's production flag (SIGMA-190) — the seed for new
 *  databases' backup retention. Editable rather than write-once. */
export async function setEnvironmentProduction(input: {
  environmentId: string;
  production: boolean;
}) {
  const { env, project, user, role } = await requireEnvMembership(input.environmentId);
  if (cpEnabled()) {
    await cpUpdateEnvironment(
      project.orgId,
      input.environmentId,
      { production: input.production },
      { name: user.name, role }
    );
  }
  await db
    .update(s.environments)
    .set({ production: input.production })
    .where(eq(s.environments.id, input.environmentId));
  await writeAudit({
    orgId: project.orgId,
    actor: user.name,
    action: input.production ? "Environment marked production" : "Environment unmarked production",
    target: `${project.name} / ${env.name}`,
  });
  revalidatePath("/dashboard", "layout");
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
  const { user, role } = await requireProjectRole(project.orgId, project.id, "Project Admin");
  return { env, project, user, role };
}

/** Attach an org server to an environment so deploys can target it. */
export async function attachServerToEnv(input: {
  environmentId: string;
  serverId: string;
}) {
  const { env, project, user, role } = await requireEnvMembership(input.environmentId);
  let serverName = input.serverId;
  if (cpEnabled()) {
    // The CP owns servers in CP mode: it 404s ids outside this org (IDOR) and
    // audits the attach. Mirror the CP server into the local servers table
    // FIRST so the env_servers.server_id FK holds (CP servers have no local
    // row otherwise), then record the CP-side attachment.
    const mirrored = await cpMirrorServer(project.orgId, input.serverId);
    serverName = mirrored.name;
    await cpAttachServer(project.orgId, input.environmentId, input.serverId, {
      name: user.name,
      role,
    });
  } else {
    // The server must belong to the same org (don't trust the client id — IDOR).
    const [sv] = await db
      .select({ orgId: s.servers.orgId, name: s.servers.name })
      .from(s.servers)
      .where(eq(s.servers.id, input.serverId));
    if (!sv || sv.orgId !== project.orgId) {
      throw new Error("Server does not belong to this organization.");
    }
    serverName = sv.name;
  }
  await db
    .insert(s.envServers)
    .values({ environmentId: input.environmentId, serverId: input.serverId })
    .onConflictDoNothing();
  await writeAudit({
    orgId: project.orgId,
    actor: user.name,
    action: "Attached server",
    target: `${serverName} → ${project.name} / ${env.name}`,
  });
  revalidatePath("/dashboard", "layout");
}

export async function detachServerFromEnv(input: {
  environmentId: string;
  serverId: string;
}) {
  const { env, project, user, role } = await requireEnvMembership(input.environmentId);
  let serverName = input.serverId;
  if (!cpEnabled()) {
    // Same ownership rule as attach: never resolve another org's server (IDOR /
    // cross-org name disclosure via the audit log).
    const [sv] = await db
      .select({ orgId: s.servers.orgId, name: s.servers.name })
      .from(s.servers)
      .where(eq(s.servers.id, input.serverId));
    if (!sv || sv.orgId !== project.orgId) {
      throw new Error("Server does not belong to this organization.");
    }
    serverName = sv.name;
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
  if (cpEnabled()) {
    // CP-side detach enforces org scoping and audits; local row mirrors it.
    await cpDetachServer(project.orgId, input.environmentId, input.serverId, {
      name: user.name,
      role,
    });
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
      target: `${serverName} ← ${project.name} / ${env.name}`,
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
  const membership = project ? await requireProjectRole(project.orgId, project.id, "Project Admin") : null;
  if (project && membership && cpEnabled()) {
    await cpDeleteEnvironment(project.orgId, input.environmentId, {
      name: membership.user.name,
      role: membership.role,
    });
  }
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
