import "server-only";
import { and, count, eq, inArray, desc } from "drizzle-orm";
import { db } from "./db";
import * as s from "./db/schema";
import { user } from "./db/auth-schema";
import { cpEnabled, cpListServers, cpServerCount, cpServerToRow } from "./cp";
import { reportCpFailure } from "./cp-sync";
import { hashInviteToken } from "../lib/invite";
import {
  UNIT_PRICE,
  CURRENCY,
  FREE_TIER_UNITS,
  summarizeUnits,
  billableUnits,
} from "@/lib/billing-units";
import { toDeployTargetServer } from "@/components/dashboard/resources/resource-meta";

// Pricing lives in lib/billing-units (mirrored from the CP weight table);
// re-exported here so existing importers keep working.
export { UNIT_PRICE, CURRENCY, FREE_TIER_UNITS } from "@/lib/billing-units";

export async function getOrgs() {
  return db.select().from(s.orgs);
}
export async function getOrg(id: string) {
  return (await db.select().from(s.orgs).where(eq(s.orgs.id, id)))[0];
}
export async function getProjects(orgId: string, visible?: Set<string> | null) {
  const rows = await db.select().from(s.projects).where(eq(s.projects.orgId, orgId));
  // P2-7 visibility: a project-scoped user sees only granted projects; null
  // (org admins / users with zero grants) means everything.
  return visible ? rows.filter((p) => visible.has(p.id)) : rows;
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
 *  query below goes through here so both modes stay consistent. When the CP
 *  is unreachable this falls back to the reconciled mirror instead of
 *  throwing or silently returning nothing — the layout banner (SIGMA-56)
 *  tells the user the view may be stale. */
export async function getServers(orgId: string) {
  if (cpEnabled()) {
    try {
      return (await cpListServers(orgId)).map(cpServerToRow);
    } catch (err) {
      reportCpFailure(orgId, err);
    }
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
  const rows = await db
    .select({
      id: user.id,
      name: user.name,
      email: user.email,
      role: s.memberships.role,
      scoped: s.memberships.scoped,
    })
    .from(s.memberships)
    .innerJoin(user, eq(s.memberships.userId, user.id))
    .where(eq(s.memberships.orgId, orgId));

  // SIGMA-311: removing a member also deletes every project grant they hold in
  // this org (removeMember, for the resurrect-on-re-invite reason in SIGMA-148)
  // and nothing anywhere records what those grants were — re-inviting restores
  // the membership but not the access. The confirmation dialog therefore has to
  // be able to say how many grants are about to be destroyed, so the count is
  // carried alongside the member rather than left for the operator to remember.
  const grants = await db
    .select({ userId: s.projectMemberships.userId })
    .from(s.projectMemberships)
    .innerJoin(s.projects, eq(s.projectMemberships.projectId, s.projects.id))
    .where(eq(s.projects.orgId, orgId));
  const perUser = new Map<string, number>();
  for (const g of grants) perUser.set(g.userId, (perUser.get(g.userId) ?? 0) + 1);

  return rows.map((r) => ({ ...r, grantCount: perUser.get(r.id) ?? 0 }));
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
        // SIGMA-335: a COUNT, not a list. This used to be
        // `cpListServers(id).then((l) => l.length)`, which made the control
        // plane build and serialise its full dashboard projection — every
        // column, the facts jsonb blob and a correlated readiness subquery per
        // row — for every org the user belongs to, on every render of the
        // layout, so that the switcher could render one integer each. A
        // consultant in six orgs of a hundred hosts moved six hundred
        // fully-projected rows per navigation to draw six numbers.
        //
        // A CP hiccup must not take down the org switcher — show 0 instead.
        counts[id] = await cpServerCount(id).catch(() => 0);
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

/** Environments a cluster could be built in, labelled with their project.
 *
 *  A cluster belongs to exactly one environment, so the create dialog needs the
 *  list; it used to come out of getCommandIndex, which also loads every server
 *  and every resource in the org to answer it. `visible` applies the P2-7
 *  project scoping (null = everything) so a project-scoped user is not offered
 *  an environment in a project they were never granted. */
export async function getClusterEnvironments(
  orgId: string,
  visible?: Set<string> | null
): Promise<{ id: string; name: string; projectName: string }[]> {
  const rows = await db
    .select({
      id: s.environments.id,
      name: s.environments.name,
      projectId: s.environments.projectId,
      projectName: s.projects.name,
    })
    .from(s.environments)
    .innerJoin(s.projects, eq(s.environments.projectId, s.projects.id))
    .where(eq(s.projects.orgId, orgId))
    .orderBy(s.projects.name, s.environments.name);
  return (visible ? rows.filter((r) => visible.has(r.projectId)) : rows).map(
    ({ id, name, projectName }) => ({ id, name, projectName })
  );
}

export type CommandIndex = {
  projects: { id: string; name: string; slug: string }[];
  environments: { id: string; name: string; projectId: string; projectName: string }[];
  servers: { id: string; name: string; type: string; region: string }[];
  resources: { id: string; name: string; kind: string; projectName: string }[];
};

/** Flat, org-scoped search index that powers the ⌘K command menu. `visible`
 *  applies the P2-7 project scoping (null = everything). */
export async function getCommandIndex(
  orgId: string,
  visible?: Set<string> | null
): Promise<CommandIndex> {
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
        projectId: s.resources.projectId,
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
  const visibleResources = (visible ? resources.filter((r) => visible.has(r.projectId)) : resources)
    .map(({ id, name, kind, projectName }) => ({ id, name, kind, projectName }));
  if (!visible) return { projects, environments, servers, resources: visibleResources };
  return {
    projects: projects.filter((p) => visible.has(p.id)),
    environments: environments.filter((e) => visible.has(e.projectId)),
    servers,
    resources: visibleResources,
  };
}

/** How many releases a resource page is allowed to load (SIGMA-329).
 *
 *  The control plane already caps release history — ListDeployments
 *  (cp/internal/store/deployments.go) defaults to 20 and refuses more than 100 —
 *  and the CP-mode page path respects that. The demo/mirror path had no bound
 *  at all, so the same page showed 20 releases against a control plane and
 *  every row ever written without one. Both modes now bound the same way. */
export const DEPLOYMENT_HISTORY_LIMIT = 25;

/** A resource's recent deployments, newest first.
 *
 *  Both the order and the bound are load-bearing. Without ORDER BY, "recent
 *  deployments" was whatever order Postgres happened to return, and without a
 *  LIMIT a resource deployed on every push hands its caller thousands of rows —
 *  which, in a server component, are serialised into the RSC payload sent to
 *  the browser. */
export async function getDeployments(resourceId: string, limit = DEPLOYMENT_HISTORY_LIMIT) {
  return db
    .select()
    .from(s.deployments)
    .where(eq(s.deployments.resourceId, resourceId))
    .orderBy(desc(s.deployments.startedAt))
    .limit(limit);
}
export async function getBillingSummary(orgId: string) {
  const all = await getServers(orgId).catch(() => []);
  // Match the CP/Paddle charge basis exactly (SIGMA-91): "connected" = running
  // servers (the CP's ConnectedServerUnits uses status='running'), and the
  // charge is only for units ABOVE the free tier (subtract), NOT every
  // connected unit once the tier is exceeded (the old cliff overstated it ~4x).
  const running = all.filter((x) => x.status === "running");
  const { lines, servers: connected, units } = summarizeUnits(running);
  const billable = billableUnits(units);
  return {
    connected,
    units,
    // Demo mode has no server-hours meter and no subscription, so there is no
    // high-water mark to bill on and no minimum to floor at: the live unit count
    // IS the billed one. Present so the view reads one field either way
    // (SIGMA-292); the window is 0 because none was applied.
    billedUnits: units,
    billingWindowHours: 0,
    billableUnits: billable,
    breakdown: lines,
    freeTier: FREE_TIER_UNITS,
    unitPrice: UNIT_PRICE,
    currency: CURRENCY,
    amount: billable * UNIT_PRICE,
    isFree: billable === 0,
  };
}

// ── Projects/Environments read models (V1-3) ───────────────────────────────

export type ProjectSummary = {
  project: typeof s.projects.$inferSelect;
  envCount: number;
  serverCount: number;
  resourceCount: number;
  statusCounts: Record<string, number>;
  /** SIGMA-314: what a project delete cascades away, by name and kind. The card
   *  already counted these rows; the delete dialog has to be able to show them,
   *  because a count alone can't tell an operator they picked the wrong card. */
  resources: { id: string; name: string; kind: string; envName: string }[];
};

/** One card per project: env/server/resource counts + resource-status breakdown. */
export async function getProjectSummaries(
  orgId: string,
  visible?: Set<string> | null
): Promise<ProjectSummary[]> {
  const projs = await getProjects(orgId, visible);
  if (projs.length === 0) return [];
  const projIds = projs.map((p) => p.id);

  // SIGMA-326: three queries for the whole page, not three PER CARD. This used
  // to be a Promise.all over the projects issuing an environments, a resources
  // and an env_servers query each, so the Projects page cost 1 + 3n round-trips
  // — and every read here is `no-store`, so an org with twenty projects paid
  // sixty-one sequential-ish round-trips on every single navigation. The rows
  // are the same rows; only the grouping moved from SQL into memory.
  const envs = await db
    .select({
      id: s.environments.id,
      name: s.environments.name,
      projectId: s.environments.projectId,
    })
    .from(s.environments)
    .where(inArray(s.environments.projectId, projIds));
  const resources = await db
    .select({
      id: s.resources.id,
      name: s.resources.name,
      kind: s.resources.kind,
      projectId: s.resources.projectId,
      environmentId: s.resources.environmentId,
      status: s.resources.status,
    })
    .from(s.resources)
    .where(inArray(s.resources.projectId, projIds));
  const envIds = envs.map((e) => e.id);
  const attachments = envIds.length
    ? await db
        .selectDistinct({
          environmentId: s.envServers.environmentId,
          serverId: s.envServers.serverId,
        })
        .from(s.envServers)
        .where(inArray(s.envServers.environmentId, envIds))
    : [];

  const envNames = new Map(envs.map((e) => [e.id, e.name]));
  const envsByProject = new Map<string, string[]>();
  for (const e of envs) {
    const list = envsByProject.get(e.projectId);
    if (list) list.push(e.id);
    else envsByProject.set(e.projectId, [e.id]);
  }
  const resourcesByProject = new Map<string, typeof resources>();
  for (const r of resources) {
    const list = resourcesByProject.get(r.projectId);
    if (list) list.push(r);
    else resourcesByProject.set(r.projectId, [r]);
  }
  const serversByEnv = new Map<string, string[]>();
  for (const a of attachments) {
    const list = serversByEnv.get(a.environmentId);
    if (list) list.push(a.serverId);
    else serversByEnv.set(a.environmentId, [a.serverId]);
  }

  return projs.map((project) => {
    const projectEnvIds = envsByProject.get(project.id) ?? [];
    const projectResources = resourcesByProject.get(project.id) ?? [];
    // Distinct across the project's environments, exactly as the per-project
    // selectDistinct did: one server attached to two of a project's
    // environments is one server on the card, not two.
    const serverIds = new Set<string>();
    for (const envId of projectEnvIds) {
      for (const sv of serversByEnv.get(envId) ?? []) serverIds.add(sv);
    }
    const statusCounts: Record<string, number> = {};
    for (const r of projectResources) {
      statusCounts[r.status] = (statusCounts[r.status] ?? 0) + 1;
    }
    return {
      project,
      envCount: projectEnvIds.length,
      serverCount: serverIds.size,
      resourceCount: projectResources.length,
      statusCounts,
      resources: projectResources.map((r) => ({
        id: r.id,
        name: r.name,
        kind: r.kind,
        envName: envNames.get(r.environmentId) ?? "",
      })),
    };
  });
}

/** Sidebar nav: each project with its environments (id + name). */
export async function getProjectsWithEnvs(orgId: string, visible?: Set<string> | null) {
  const projs = await getProjects(orgId, visible);
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
  if (rows.length === 0) return [];
  // SIGMA-326: one GROUP BY instead of a $count per server. The previous shape
  // was 1 + n round-trips for a page that shows n rows, which is the same N+1
  // getOrgResources had — a fleet of a hundred hosts made the Servers page a
  // hundred and one queries deep, on every navigation, for n integers.
  const counts = await db
    .select({ serverId: s.resources.serverId, n: count() })
    .from(s.resources)
    .where(
      inArray(
        s.resources.serverId,
        rows.map((sv) => sv.id)
      )
    )
    .groupBy(s.resources.serverId);
  const byServer = new Map<string, number>();
  for (const c of counts) if (c.serverId) byServer.set(c.serverId, Number(c.n));
  return rows.map((sv) => ({ ...sv, resourceCount: byServer.get(sv.id) ?? 0 }));
}

/** Resources scheduled on a server, with their project + environment names. */
/** Resources scheduled on a server, with project/env names. `visible` applies
 *  P2-7 project scoping (null = everything): servers are org-wide, but a
 *  project-scoped user must not see the resource names/kinds/statuses or owning
 *  project/env names of projects they were never granted (SIGMA-149). */
export async function getServerHosted(serverId: string, visible?: Set<string> | null) {
  const rows = await db
    .select({
      id: s.resources.id,
      name: s.resources.name,
      kind: s.resources.kind,
      status: s.resources.status,
      projectId: s.resources.projectId,
      projectName: s.projects.name,
      envName: s.environments.name,
    })
    .from(s.resources)
    .innerJoin(s.projects, eq(s.resources.projectId, s.projects.id))
    .innerJoin(s.environments, eq(s.resources.environmentId, s.environments.id))
    .where(eq(s.resources.serverId, serverId));
  const scoped = visible ? rows.filter((r) => visible.has(r.projectId)) : rows;
  // Return the view shape without the internal projectId used only for scoping.
  return scoped.map((r) => ({
    id: r.id,
    name: r.name,
    kind: r.kind,
    status: r.status,
    projectName: r.projectName,
    envName: r.envName,
  }));
}

export type OrgResource = typeof s.resources.$inferSelect & {
  projectName: string;
  envName: string;
  latestDeploy: typeof s.deployments.$inferSelect | null;
};

/** Nested project → environment → server tree for the deploy wizard target step.
 *  `visible` applies the P2-7 project scoping (null = everything) so a
 *  project-scoped user's target picker only offers granted projects (SIGMA-75). */
export async function getDeployTargets(orgId: string, visible?: Set<string> | null) {
  const projs = await getProjects(orgId, visible);
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
              // Shared with the project page's own target builder so the two
              // cannot carry different fields (SIGMA-304). It copies status
              // (SIGMA-203) and the GPU inventory (SIGMA-214) and nothing else
              // out of the facts blob.
              .map(toDeployTargetServer),
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

  // The host's STATUS travels with its name (SIGMA-251). A resource on a server
  // that stopped heartbeating has nothing to say for itself — no error, because
  // nothing failed; no deployment, because none was ever picked up — so the page
  // needs the one fact that explains the silence. The mirror's status is
  // reconciled from the control plane (cp-sync writes it on every sync), so it
  // is the same verdict the servers page renders.
  let server: { id: string; name: string; type: string; status: string } | null = null;
  if (row.resource.serverId) {
    const [sv] = await db
      .select({
        id: s.servers.id,
        name: s.servers.name,
        type: s.servers.type,
        status: s.servers.status,
      })
      .from(s.servers)
      .where(eq(s.servers.id, row.resource.serverId));
    server = sv ?? null;
  }
  // The other kind of target. A cluster workload has no server — the scheduler
  // picks the node — so a page that only ever resolved a server showed one
  // deployed into a cluster as running nowhere (SIGMA-215). Demo-only by
  // construction: the column is written only when there is no control plane,
  // because in CP mode the control plane holds the target and this row is a
  // read model of it.
  let cluster: { id: string; name: string } | null = null;
  if (row.resource.clusterId) {
    const [c] = await db
      .select({ id: s.clusters.id, name: s.clusters.name })
      .from(s.clusters)
      .where(eq(s.clusters.id, row.resource.clusterId));
    cluster = c ?? null;
  }
  // Bounded, newest first (SIGMA-329). This runs in a server component, so an
  // unbounded select does not merely cost a big query — every row it returns is
  // serialised into the RSC payload and sent to the browser to render a panel
  // that shows a couple of dozen releases. The cap matches the control plane's
  // own release-history cap so both modes agree about how much history a
  // resource has.
  const deployments = await db
    .select()
    .from(s.deployments)
    .where(eq(s.deployments.resourceId, resourceId))
    .orderBy(desc(s.deployments.startedAt))
    .limit(DEPLOYMENT_HISTORY_LIMIT);

  return {
    resource: row.resource,
    projectName: row.projectName,
    orgId: row.orgId,
    envName: row.envName,
    server,
    cluster,
    deployments,
  };
}

/** Every resource in the org, with project/environment names and its most
 *  recent deployment. Powers the Overview + Resources pages. `visible` applies
 *  the P2-7 project scoping (null = everything): a project-scoped user must not
 *  see resources — DB/S3 metadata, deploy history, logs — in projects they were
 *  never granted, even inside their own org (SIGMA-75). */
export async function getOrgResources(
  orgId: string,
  visible?: Set<string> | null
): Promise<OrgResource[]> {
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
  const scoped = visible ? rows.filter((row) => visible.has(row.resource.projectId)) : rows;
  if (scoped.length === 0) return [];

  // SIGMA-326: ONE query for every resource's most recent deployment.
  //
  // This used to be a `Promise.all` over `scoped` issuing a `… WHERE
  // resource_id = ? ORDER BY started_at DESC LIMIT 1` per resource, so the
  // Overview and Resources pages cost 1 + n round-trips against the web
  // database before their first byte of HTML. Both pages are `no-store`, so an
  // org with six hundred resources paid six hundred and one queries on every
  // navigation and a handful of concurrent users queued behind the pool.
  //
  // DISTINCT ON is the Postgres way to say "the first row of each group": the
  // ORDER BY must lead with the DISTINCT ON expression, and the trailing
  // `started_at DESC` then decides WHICH row of each group survives — the same
  // row the per-resource LIMIT 1 picked.
  const latest = await db
    .selectDistinctOn([s.deployments.resourceId])
    .from(s.deployments)
    .where(
      inArray(
        s.deployments.resourceId,
        scoped.map((row) => row.resource.id)
      )
    )
    .orderBy(s.deployments.resourceId, desc(s.deployments.startedAt));
  const latestByResource = new Map(latest.map((d) => [d.resourceId, d]));

  return scoped.map((row) => ({
    ...row.resource,
    projectName: row.projectName,
    envName: row.envName,
    latestDeploy: latestByResource.get(row.resource.id) ?? null,
  }));
}

/** P2-7b: pending (unaccepted, unrevoked) invitations for an org's Members tab. */
export async function getPendingInvites(orgId: string) {
  return db
    .select({
      id: s.invitations.id,
      email: s.invitations.email,
      role: s.invitations.role,
      invitedBy: s.invitations.invitedBy,
      expiresAt: s.invitations.expiresAt,
      createdAt: s.invitations.createdAt,
    })
    .from(s.invitations)
    .where(and(eq(s.invitations.orgId, orgId), eq(s.invitations.status, "pending")))
    .orderBy(desc(s.invitations.createdAt));
}

/** P2-7b: resolve an invite by its raw token (hashed for lookup) for the accept
 *  page. Returns the invite plus its org name, or null when unknown. Read-only —
 *  status/expiry are judged by the caller so the page can show honest copy. */
export async function getInviteByToken(rawToken: string) {
  const hash = hashInviteToken(rawToken);
  const [inv] = await db
    .select({
      id: s.invitations.id,
      orgId: s.invitations.orgId,
      orgName: s.orgs.name,
      email: s.invitations.email,
      role: s.invitations.role,
      status: s.invitations.status,
      expiresAt: s.invitations.expiresAt,
    })
    .from(s.invitations)
    .innerJoin(s.orgs, eq(s.invitations.orgId, s.orgs.id))
    .where(eq(s.invitations.tokenHash, hash));
  return inv ?? null;
}
