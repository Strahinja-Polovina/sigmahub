import "server-only";
import { eq, inArray, desc } from "drizzle-orm";
import { db } from "./db";
import * as s from "./db/schema";
import { user } from "./db/auth-schema";
import { cpEnabled, cpListServers, cpServerToRow } from "./cp";

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
/** Org servers. CP mode reads the control plane (mapped onto the local row
 *  shape); demo mode reads the simulated PGlite rows. Every server-reading
 *  query below goes through here so both modes stay consistent. */
export async function getServers(orgId: string) {
  if (cpEnabled()) {
    return (await cpListServers(orgId)).map(cpServerToRow);
  }
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

/** Server count per org, for the org switcher (id → count). */
export async function getServerCounts(
  orgIds: string[]
): Promise<Record<string, number>> {
  const counts: Record<string, number> = {};
  for (const id of orgIds) counts[id] = 0;
  if (orgIds.length === 0) return counts;
  if (cpEnabled()) {
    await Promise.all(
      orgIds.map(async (id) => {
        // A CP hiccup must not take down the org switcher — show 0 instead.
        counts[id] = await cpListServers(id).then((l) => l.length).catch(() => 0);
      })
    );
    return counts;
  }
  const rows = await db
    .select({ orgId: s.servers.orgId })
    .from(s.servers)
    .where(inArray(s.servers.orgId, orgIds));
  for (const r of rows) counts[r.orgId] = (counts[r.orgId] ?? 0) + 1;
  return counts;
}

export type CommandIndex = {
  projects: { id: string; name: string; slug: string }[];
  environments: { id: string; name: string; projectId: string; projectName: string }[];
  servers: { id: string; name: string; type: string; region: string }[];
  resources: { id: string; name: string; kind: string; projectName: string }[];
};

/** Flat, org-scoped search index that powers the ⌘K command menu. */
export async function getCommandIndex(orgId: string): Promise<CommandIndex> {
  const [projects, environments, serverRows, resources] = await Promise.all([
    db
      .select({ id: s.projects.id, name: s.projects.name, slug: s.projects.slug })
      .from(s.projects)
      .where(eq(s.projects.orgId, orgId)),
    db
      .select({
        id: s.environments.id,
        name: s.environments.name,
        projectId: s.environments.projectId,
        projectName: s.projects.name,
      })
      .from(s.environments)
      .innerJoin(s.projects, eq(s.environments.projectId, s.projects.id))
      .where(eq(s.projects.orgId, orgId)),
    // Layout-level ⌘K menu: a CP hiccup must not error every dashboard page,
    // so degrade to no server entries (mirrors getServerCounts).
    getServers(orgId).catch(() => []),
    db
      .select({
        id: s.resources.id,
        name: s.resources.name,
        kind: s.resources.kind,
        projectName: s.projects.name,
      })
      .from(s.resources)
      .innerJoin(s.projects, eq(s.resources.projectId, s.projects.id))
      .where(eq(s.projects.orgId, orgId)),
  ]);
  const servers = serverRows.map((sv) => ({
    id: sv.id,
    name: sv.name,
    type: sv.type,
    region: sv.region,
  }));
  return { projects, environments, servers, resources };
}

export async function getDeployments(resourceId: string) {
  return db
    .select()
    .from(s.deployments)
    .where(eq(s.deployments.resourceId, resourceId));
}
export async function getBillingSummary(orgId: string) {
  const all = await getServers(orgId).catch(() => []);
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
  env: typeof s.environments.$inferSelect,
  orgServers: (typeof s.servers.$inferSelect)[]
): Promise<EnvPanel> {
  // env_servers is the local mirror of the CP attachment rows; the server
  // rows themselves come from getServers so CP mode renders real CP servers.
  const attached = await db
    .select({ serverId: s.envServers.serverId })
    .from(s.envServers)
    .where(eq(s.envServers.environmentId, env.id));
  const attachedIds = new Set(attached.map((a) => a.serverId));
  const servers = orgServers.filter((sv) => attachedIds.has(sv.id));
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
  return { env, servers, resources: withDeploy };
}

/** Project detail: each environment with its attached servers + resources
 *  (each resource carrying its most-recent deployment). */
export async function getEnvironmentPanels(projectId: string): Promise<EnvPanel[]> {
  const project = await getProject(projectId);
  const orgServers = project ? await getServers(project.orgId) : [];
  const envs = await db
    .select()
    .from(s.environments)
    .where(eq(s.environments.projectId, projectId))
    .orderBy(s.environments.name);
  return Promise.all(envs.map((env) => buildEnvPanel(env, orgServers)));
}

/** Single-environment detail panel. */
export async function getEnvironmentPanel(
  envId: string
): Promise<EnvPanel | undefined> {
  const [env] = await db
    .select()
    .from(s.environments)
    .where(eq(s.environments.id, envId));
  if (!env) return undefined;
  const project = await getProject(env.projectId);
  const orgServers = project ? await getServers(project.orgId) : [];
  return buildEnvPanel(env, orgServers);
}

// ── Servers read models (V1-4) ─────────────────────────────────────────────

export type ServerWithCount = typeof s.servers.$inferSelect & {
  resourceCount: number;
};

/** Org servers with how many resources are scheduled on each. */
export async function getServersWithCounts(
  orgId: string
): Promise<ServerWithCount[]> {
  const rows = await db
    .select()
    .from(s.servers)
    .where(eq(s.servers.orgId, orgId))
    .orderBy(s.servers.connectedAt);
  return Promise.all(
    rows.map(async (sv) => ({
      ...sv,
      resourceCount: await db.$count(
        s.resources,
        eq(s.resources.serverId, sv.id)
      ),
    }))
  );
}

/** Resources scheduled on a server, with their project + environment names. */
export async function getServerHosted(serverId: string) {
  return db
    .select({
      id: s.resources.id,
      name: s.resources.name,
      kind: s.resources.kind,
      status: s.resources.status,
      projectName: s.projects.name,
      envName: s.environments.name,
    })
    .from(s.resources)
    .innerJoin(s.projects, eq(s.resources.projectId, s.projects.id))
    .innerJoin(s.environments, eq(s.resources.environmentId, s.environments.id))
    .where(eq(s.resources.serverId, serverId));
}

export type OrgResource = typeof s.resources.$inferSelect & {
  projectName: string;
  envName: string;
  latestDeploy: typeof s.deployments.$inferSelect | null;
};

/** Nested project → environment → server tree for the deploy wizard target step. */
export async function getDeployTargets(orgId: string) {
  const projs = await getProjects(orgId);
  const orgServers = await getServers(orgId);
  const byId = new Map(orgServers.map((sv) => [sv.id, sv]));
  return Promise.all(
    projs.map(async (p) => {
      const envs = await db
        .select({ id: s.environments.id, name: s.environments.name })
        .from(s.environments)
        .where(eq(s.environments.projectId, p.id))
        .orderBy(s.environments.name);
      const environments = await Promise.all(
        envs.map(async (e) => {
          const attached = await db
            .select({ serverId: s.envServers.serverId })
            .from(s.envServers)
            .where(eq(s.envServers.environmentId, e.id));
          return {
            ...e,
            servers: attached
              .map((a) => byId.get(a.serverId))
              .filter((sv): sv is NonNullable<typeof sv> => Boolean(sv))
              .map((sv) => ({
                id: sv.id,
                name: sv.name,
                type: sv.type,
                provider: sv.provider,
                region: sv.region,
              })),
          };
        })
      );
      return { id: p.id, name: p.name, environments };
    })
  );
}

/** Resource detail: the row + project/env/server names + full deploy history. */
export async function getResourceDetail(resourceId: string) {
  const [row] = await db
    .select({
      resource: s.resources,
      projectName: s.projects.name,
      orgId: s.projects.orgId,
      envName: s.environments.name,
    })
    .from(s.resources)
    .innerJoin(s.projects, eq(s.resources.projectId, s.projects.id))
    .innerJoin(s.environments, eq(s.resources.environmentId, s.environments.id))
    .where(eq(s.resources.id, resourceId));
  if (!row) return undefined;

  let server: { id: string; name: string; type: string } | null = null;
  if (row.resource.serverId) {
    const [sv] = await db
      .select({ id: s.servers.id, name: s.servers.name, type: s.servers.type })
      .from(s.servers)
      .where(eq(s.servers.id, row.resource.serverId));
    server = sv ?? null;
  }
  const deployments = await db
    .select()
    .from(s.deployments)
    .where(eq(s.deployments.resourceId, resourceId))
    .orderBy(desc(s.deployments.startedAt));

  return {
    resource: row.resource,
    projectName: row.projectName,
    orgId: row.orgId,
    envName: row.envName,
    server,
    deployments,
  };
}

/** Every resource in the org, with project/environment names and its most
 *  recent deployment. Powers the Overview + Resources pages. */
export async function getOrgResources(orgId: string): Promise<OrgResource[]> {
  const rows = await db
    .select({
      resource: s.resources,
      projectName: s.projects.name,
      envName: s.environments.name,
    })
    .from(s.resources)
    .innerJoin(s.projects, eq(s.resources.projectId, s.projects.id))
    .innerJoin(s.environments, eq(s.resources.environmentId, s.environments.id))
    .where(eq(s.projects.orgId, orgId));
  return Promise.all(
    rows.map(async (row) => {
      const [latest] = await db
        .select()
        .from(s.deployments)
        .where(eq(s.deployments.resourceId, row.resource.id))
        .orderBy(desc(s.deployments.startedAt))
        .limit(1);
      return {
        ...row.resource,
        projectName: row.projectName,
        envName: row.envName,
        latestDeploy: latest ?? null,
      };
    })
  );
}
