import "server-only";

// CP ↔ local read-model reconciliation (SIGMA-56). In CP mode the dashboard
// keeps local mirror rows (projects/environments/resources/servers) purely as
// read-models under the same ids as the CP entities. Two things break that
// mirror: a request dying between the CP write and the local write (drift),
// and entities the CP creates on its own — preview environments (P1-12) never
// get a local row at all. This module makes the CP the source of truth: list
// everything it owns, upsert those rows, tombstone local rows it no longer
// knows. It runs throttled from the dashboard layout, so drift self-heals on
// navigation without a background daemon, and its health feeds the
// "control plane unreachable" banner instead of pages silently rendering
// stale-empty lists.

import { and, eq, inArray, notInArray } from "drizzle-orm";
import { db } from "./db";
import * as s from "./db/schema";
import {
  cpEnabled,
  cpEnvServerIds,
  cpListEnvironments,
  cpListOrgDeployments,
  cpListProjects,
  cpListResources,
  cpListServers,
  cpServerToRow,
} from "./cp";
import {
  environmentMirrorRow,
  localDeployStatus,
  projectMirrorRow,
  resourceMirrorRow,
  staleIds,
} from "@/lib/mirror-sync";

export type CpSyncStatus = {
  /** False in demo mode: no CP, nothing to reconcile, no banner. */
  enabled: boolean;
  /** True while the CP is reachable (no failure since the last success). */
  healthy: boolean;
  /** Last successful full sync, ISO timestamp. */
  lastSyncAt: string | null;
  error: string | null;
};

const SYNC_INTERVAL_MS = 30_000;
// How long a page render waits for an in-flight sync before proceeding with
// the current mirror — a hung CP must not hold the dashboard hostage.
const SYNC_WAIT_MS = 8_000;

type OrgSyncState = {
  inFlight: Promise<void> | null;
  lastAttemptAt: number;
  lastSyncAt: Date | null;
  error: string | null;
};

const syncStates = new Map<string, OrgSyncState>();

function orgState(orgId: string): OrgSyncState {
  let st = syncStates.get(orgId);
  if (!st) {
    st = { inFlight: null, lastAttemptAt: 0, lastSyncAt: null, error: null };
    syncStates.set(orgId, st);
  }
  return st;
}

/** Read paths that talk to the CP directly (e.g. the servers list) report
 *  failures here so the banner also reflects outages seen outside the sync. */
export function reportCpFailure(orgId: string, err: unknown): void {
  orgState(orgId).error = err instanceof Error ? err.message : String(err);
}

function statusOf(st: OrgSyncState): CpSyncStatus {
  return {
    enabled: true,
    healthy: st.error === null,
    lastSyncAt: st.lastSyncAt ? st.lastSyncAt.toISOString() : null,
    error: st.error,
  };
}

/** One full reconcile pass for an org. Every CP list is fetched before the
 *  first write: tombstoning from partial data would delete rows the CP still
 *  owns, so any fetch failure aborts the pass with the mirror untouched. */
export async function syncOrgMirror(orgId: string): Promise<void> {
  const [cpServers, cpProjects, cpResources, cpDeploys] = await Promise.all([
    cpListServers(orgId),
    cpListProjects(orgId),
    cpListResources(orgId),
    cpListOrgDeployments(orgId),
  ]);
  const cpEnvs = (
    await Promise.all(cpProjects.map((p) => cpListEnvironments(orgId, p.id)))
  ).flat();
  const envAttachments = await Promise.all(
    cpEnvs.map(async (env) => ({
      envId: env.id,
      serverIds: await cpEnvServerIds(orgId, env.id),
    }))
  );

  // Existing mirror rows, org-scoped: locally-kept fields (slug, demo status)
  // survive the upsert, and the id sets feed the tombstone diffs. Scoping by
  // org here is what makes the deletes below safe.
  const [localServers, localProjects, localEnvs, localResources] =
    await Promise.all([
      db.select({ id: s.servers.id }).from(s.servers).where(eq(s.servers.orgId, orgId)),
      db.select().from(s.projects).where(eq(s.projects.orgId, orgId)),
      db
        .select({ id: s.environments.id })
        .from(s.environments)
        .innerJoin(s.projects, eq(s.environments.projectId, s.projects.id))
        .where(eq(s.projects.orgId, orgId)),
      db
        .select({
          id: s.resources.id,
          status: s.resources.status,
          repo: s.resources.repo,
          domain: s.resources.domain,
          version: s.resources.version,
          lastDeployAt: s.resources.lastDeployAt,
        })
        .from(s.resources)
        .innerJoin(s.projects, eq(s.resources.projectId, s.projects.id))
        .where(eq(s.projects.orgId, orgId)),
    ]);

  // 1. Servers first — resources and env_servers FK onto them.
  for (const srv of cpServers) {
    const row = cpServerToRow(srv);
    await db
      .insert(s.servers)
      .values(row)
      .onConflictDoUpdate({
        target: s.servers.id,
        set: {
          name: row.name,
          type: row.type,
          source: row.source,
          provider: row.provider,
          region: row.region,
          status: row.status,
          agentVersion: row.agentVersion,
          ip: row.ip,
          meshIp: row.meshIp,
          cpu: row.cpu,
          memGb: row.memGb,
          // Detected facts and the gate's verdict move — a driver appears, a
          // disk grows, a type is changed — so the mirror has to follow them
          // like every other mutable column here, or it freezes at whatever
          // the host looked like the first time it was synced.
          facts: row.facts,
          incompatibleReasons: row.incompatibleReasons,
          // A decommission starts and ends on the control plane's clock, so a
          // mirror that froze this would show a stale "Decommissioning" pill —
          // or miss one entirely — on every page that reads the mirror.
          decommissionStartedAt: row.decommissionStartedAt,
          decommissionPurgeVolumes: row.decommissionPurgeVolumes,
        },
      });
  }

  // 2. Projects (delete cascades their envs/resources).
  const projectsById = new Map(localProjects.map((p) => [p.id, p]));
  for (const p of cpProjects) {
    const row = projectMirrorRow(p, projectsById.get(p.id));
    await db
      .insert(s.projects)
      .values(row)
      .onConflictDoUpdate({
        target: s.projects.id,
        set: { name: row.name, slug: row.slug, description: row.description },
      });
  }

  // 3. Environments — this is what makes CP-created preview envs (pr-<n>)
  // visible in the project pages.
  for (const env of cpEnvs) {
    const row = environmentMirrorRow(env);
    await db
      .insert(s.environments)
      .values(row)
      .onConflictDoUpdate({
        target: s.environments.id,
        set: { name: row.name, projectId: row.projectId, production: row.production },
      });
  }

  // 4. Resources. The latest CP deployment per resource feeds version and
  // lastDeployAt (SIGMA-161 — both previously froze at resource creation).
  const latestByResource = new Map(cpDeploys.latest.map((d) => [d.resourceId, d]));
  const resourcesById = new Map(localResources.map((r) => [r.id, r]));
  for (const res of cpResources) {
    const row = resourceMirrorRow(res, resourcesById.get(res.id), latestByResource.get(res.id));
    await db
      .insert(s.resources)
      .values(row)
      .onConflictDoUpdate({
        target: s.resources.id,
        set: {
          projectId: row.projectId,
          environmentId: row.environmentId,
          serverId: row.serverId,
          name: row.name,
          kind: row.kind,
          status: row.status,
          repo: row.repo,
          domain: row.domain,
          ephemeral: row.ephemeral,
          version: row.version,
          lastDeployAt: row.lastDeployAt,
        },
      });
  }

  // 4b. Deployments — mirror the CP's recent + latest rows into the local
  // deployments table every existing surface already reads (activity feed,
  // Active deploys stat, per-resource latestDeploy). Only rows whose resource
  // exists locally (FK); dedup by id (SIGMA-161).
  const localResourceIds = new Set(cpResources.map((r) => r.id));
  const deployRows = new Map<string, (typeof cpDeploys.recent)[number]>();
  for (const d of [...cpDeploys.recent, ...cpDeploys.latest]) {
    if (localResourceIds.has(d.resourceId)) deployRows.set(d.id, d);
  }
  for (const d of deployRows.values()) {
    await db
      .insert(s.deployments)
      .values({
        id: d.id,
        resourceId: d.resourceId,
        sha: d.gitSha ?? "",
        status: localDeployStatus(d.status),
        author: d.createdBy ?? "",
        durationSec: d.durationSeconds ?? 0,
        startedAt: new Date(d.startedAt ?? d.createdAt),
      })
      .onConflictDoUpdate({
        target: s.deployments.id,
        set: {
          status: localDeployStatus(d.status),
          durationSec: d.durationSeconds ?? 0,
          startedAt: new Date(d.startedAt ?? d.createdAt),
        },
      });
  }

  // 5. Env↔server attachments, replaced per environment.
  for (const { envId, serverIds } of envAttachments) {
    for (const serverId of serverIds) {
      await db
        .insert(s.envServers)
        .values({ environmentId: envId, serverId })
        .onConflictDoNothing();
    }
    await db
      .delete(s.envServers)
      .where(
        serverIds.length
          ? and(
              eq(s.envServers.environmentId, envId),
              notInArray(s.envServers.serverId, serverIds)
            )
          : eq(s.envServers.environmentId, envId)
      );
  }

  // 6. Tombstones — explicit stale-id lists (org-scoped above), never a bare
  // NOT IN against the whole table.
  const staleServers = staleIds(
    localServers.map((r) => r.id),
    cpServers.map((r) => r.id)
  );
  if (staleServers.length) {
    await db
      .delete(s.servers)
      .where(and(eq(s.servers.orgId, orgId), inArray(s.servers.id, staleServers)));
  }
  const staleProjects = staleIds(
    localProjects.map((r) => r.id),
    cpProjects.map((r) => r.id)
  );
  if (staleProjects.length) {
    await db
      .delete(s.projects)
      .where(and(eq(s.projects.orgId, orgId), inArray(s.projects.id, staleProjects)));
  }
  const staleEnvs = staleIds(
    localEnvs.map((r) => r.id),
    cpEnvs.map((r) => r.id)
  );
  if (staleEnvs.length) {
    await db.delete(s.environments).where(inArray(s.environments.id, staleEnvs));
  }
  const staleResources = staleIds(
    localResources.map((r) => r.id),
    cpResources.map((r) => r.id)
  );
  if (staleResources.length) {
    await db.delete(s.resources).where(inArray(s.resources.id, staleResources));
  }
}

/** Throttled entry point the dashboard layout awaits: at most one sync per
 *  org per interval, concurrent renders share the in-flight pass, and a slow
 *  CP only delays the page up to SYNC_WAIT_MS. Never throws — failures land
 *  in the returned status (and the banner), not on the page. */
export async function maybeSyncOrgMirror(orgId: string): Promise<CpSyncStatus> {
  if (!cpEnabled()) {
    return { enabled: false, healthy: true, lastSyncAt: null, error: null };
  }
  const st = orgState(orgId);
  const now = Date.now();
  if (!st.inFlight && now - st.lastAttemptAt >= SYNC_INTERVAL_MS) {
    st.lastAttemptAt = now;
    st.inFlight = syncOrgMirror(orgId)
      .then(() => {
        st.lastSyncAt = new Date();
        st.error = null;
      })
      .catch((err) => {
        st.error = err instanceof Error ? err.message : String(err);
      })
      .finally(() => {
        st.inFlight = null;
      });
  }
  if (st.inFlight) {
    await Promise.race([
      st.inFlight,
      new Promise((resolve) => setTimeout(resolve, SYNC_WAIT_MS)),
    ]);
  }
  return statusOf(st);
}
