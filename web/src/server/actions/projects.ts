"use server";

import { revalidatePath } from "next/cache";
import { eq } from "drizzle-orm";
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
