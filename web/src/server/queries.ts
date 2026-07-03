import "server-only";
import { eq, inArray, desc } from "drizzle-orm";
import { db } from "./db";
import * as s from "./db/schema";
import { user } from "./db/auth-schema";

export const UNIT_PRICE = 5;
export const FREE_TIER_SERVERS = 3;
export const CURRENCY = "EUR";

export async function getOrgs() {
  return db.select().from(s.orgs);
}
export async function getOrg(id: string) {
  return (await db.select().from(s.orgs).where(eq(s.orgs.id, id)))[0];
}
export async function getProjects(orgId: string) {
  return db.select().from(s.projects).where(eq(s.projects.orgId, orgId));
}
export async function getProject(id: string) {
  return (await db.select().from(s.projects).where(eq(s.projects.id, id)))[0];
}
export async function getEnvironments(projectId: string) {
  return db
    .select()
    .from(s.environments)
    .where(eq(s.environments.projectId, projectId));
}
export async function getEnvironment(id: string) {
  return (
    await db.select().from(s.environments).where(eq(s.environments.id, id))
  )[0];
}
export async function getServers(orgId: string) {
  return db.select().from(s.servers).where(eq(s.servers.orgId, orgId));
}
export async function getServer(id: string) {
  return (await db.select().from(s.servers).where(eq(s.servers.id, id)))[0];
}
export async function getResources(environmentId: string) {
  return db
    .select()
    .from(s.resources)
    .where(eq(s.resources.environmentId, environmentId));
}
export async function getResourcesByProject(projectId: string) {
  return db
    .select()
    .from(s.resources)
    .where(eq(s.resources.projectId, projectId));
}
export async function getResource(id: string) {
  return (await db.select().from(s.resources).where(eq(s.resources.id, id)))[0];
}
export async function getMembers(orgId: string) {
  return db
    .select({
      id: user.id,
      name: user.name,
      email: user.email,
      role: s.memberships.role,
    })
    .from(s.memberships)
    .innerJoin(user, eq(s.memberships.userId, user.id))
    .where(eq(s.memberships.orgId, orgId));
}
export async function getDeployments(resourceId: string) {
  return db
    .select()
    .from(s.deployments)
    .where(eq(s.deployments.resourceId, resourceId));
}
export async function getBillingSummary(orgId: string) {
  const all = await db.select().from(s.servers).where(eq(s.servers.orgId, orgId));
  const connected = all.filter((x) => x.status !== "provisioning").length;
  const isFree = connected <= FREE_TIER_SERVERS;
  return {
    connected,
    freeTier: FREE_TIER_SERVERS,
    unitPrice: UNIT_PRICE,
    currency: CURRENCY,
    amount: isFree ? 0 : connected * UNIT_PRICE,
    isFree,
  };
}

// ── Projects/Environments read models (V1-3) ───────────────────────────────

export type ProjectSummary = {
  project: typeof s.projects.$inferSelect;
  envCount: number;
  serverCount: number;
  resourceCount: number;
  statusCounts: Record<string, number>;
};

/** One card per project: env/server/resource counts + resource-status breakdown. */
export async function getProjectSummaries(orgId: string): Promise<ProjectSummary[]> {
  const projs = await getProjects(orgId);
  return Promise.all(
    projs.map(async (project) => {
      const envs = await db
        .select({ id: s.environments.id })
        .from(s.environments)
        .where(eq(s.environments.projectId, project.id));
      const envIds = envs.map((e) => e.id);
      const resources = await db
        .select({ status: s.resources.status })
        .from(s.resources)
        .where(eq(s.resources.projectId, project.id));
      const serverRows = envIds.length
        ? await db
            .selectDistinct({ serverId: s.envServers.serverId })
            .from(s.envServers)
            .where(inArray(s.envServers.environmentId, envIds))
        : [];
      const statusCounts: Record<string, number> = {};
      for (const r of resources) statusCounts[r.status] = (statusCounts[r.status] ?? 0) + 1;
      return {
        project,
        envCount: envs.length,
        serverCount: serverRows.length,
        resourceCount: resources.length,
        statusCounts,
      };
    })
  );
}

/** Sidebar nav: each project with its environments (id + name). */
export async function getProjectsWithEnvs(orgId: string) {
  const projs = await getProjects(orgId);
  return Promise.all(
    projs.map(async (p) => ({
      id: p.id,
      name: p.name,
      environments: await db
        .select({ id: s.environments.id, name: s.environments.name })
        .from(s.environments)
        .where(eq(s.environments.projectId, p.id))
        .orderBy(s.environments.name),
    }))
  );
}

export type EnvPanel = {
  env: typeof s.environments.$inferSelect;
  servers: (typeof s.servers.$inferSelect)[];
  resources: (typeof s.resources.$inferSelect & {
    latestDeploy: typeof s.deployments.$inferSelect | null;
  })[];
};

async function buildEnvPanel(
  env: typeof s.environments.$inferSelect
): Promise<EnvPanel> {
  const serverRows = await db
    .select()
    .from(s.servers)
    .innerJoin(s.envServers, eq(s.envServers.serverId, s.servers.id))
    .where(eq(s.envServers.environmentId, env.id));
  const resources = await db
    .select()
    .from(s.resources)
    .where(eq(s.resources.environmentId, env.id));
  const withDeploy = await Promise.all(
    resources.map(async (r) => {
      const [latest] = await db
        .select()
        .from(s.deployments)
        .where(eq(s.deployments.resourceId, r.id))
        .orderBy(desc(s.deployments.startedAt))
        .limit(1);
      return { ...r, latestDeploy: latest ?? null };
    })
  );
  return { env, servers: serverRows.map((row) => row.servers), resources: withDeploy };
}

/** Project detail: each environment with its attached servers + resources
 *  (each resource carrying its most-recent deployment). */
export async function getEnvironmentPanels(projectId: string): Promise<EnvPanel[]> {
  const envs = await db
    .select()
    .from(s.environments)
    .where(eq(s.environments.projectId, projectId))
    .orderBy(s.environments.name);
  return Promise.all(envs.map(buildEnvPanel));
}

/** Single-environment detail panel. */
export async function getEnvironmentPanel(
  envId: string
): Promise<EnvPanel | undefined> {
  const [env] = await db
    .select()
    .from(s.environments)
    .where(eq(s.environments.id, envId));
  return env ? buildEnvPanel(env) : undefined;
}
