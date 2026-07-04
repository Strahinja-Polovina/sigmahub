"use server";

import { revalidatePath } from "next/cache";
import { eq } from "drizzle-orm";
import { db } from "../db";
import * as s from "../db/schema";
import { requireMembership } from "../active-org";
import { getProject } from "../queries";
import { rid, slugify } from "@/lib/ids";

export async function createProject(input: {
  orgId: string;
  name: string;
  description?: string;
}) {
  await requireMembership(input.orgId);
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
  await requireMembership(project.orgId);
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
  revalidatePath("/dashboard", "layout");
}

export async function deleteProject(input: { projectId: string }) {
  const project = await getProject(input.projectId);
  if (!project) return;
  await requireMembership(project.orgId);
  // FK cascade removes environments, env_servers, resources and deployments.
  await db.delete(s.projects).where(eq(s.projects.id, input.projectId));
  revalidatePath("/dashboard", "layout");
}

export async function createEnvironment(input: {
  projectId: string;
  name: string;
}) {
  const project = await getProject(input.projectId);
  if (!project) throw new Error("Project not found.");
  await requireMembership(project.orgId);
  const name = input.name.trim();
  if (!name) throw new Error("Environment name is required.");
  const id = rid("env");
  await db.insert(s.environments).values({ id, projectId: input.projectId, name });
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
  if (project) await requireMembership(project.orgId);
  await db.delete(s.environments).where(eq(s.environments.id, input.environmentId));
  revalidatePath("/dashboard", "layout");
}
