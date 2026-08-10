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

import { and, eq, getTableColumns, inArray, or, sql, type SQL } from "drizzle-orm";
import type { PgTable } from "drizzle-orm/pg-core";
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

// ── Batched-write helpers (SIGMA-327) ──────────────────────────────────────
//
// This module used to write the mirror one awaited statement at a time: an
// upsert per server, per project, per environment, per resource, per deployment
// and per env↔server attachment, plus a tombstone DELETE per environment. An
// org with 20 projects, 60 environments, 50 servers and 500 resources cost
// ~1,250 sequential round-trips — several seconds against a managed database,
// on the render path of the dashboard layout, for a user who is only opening a
// page. Worse, it paid that price whether or not anything had CHANGED: a
// steady-state org rewrote every row it owned every 30 seconds.
//
// So the pass now does two things differently. It DIFFS each CP row against the
// mirror row already loaded above and writes only the ones whose mirrored
// columns actually moved — a quiet org issues no writes at all. And what is
// left goes out as one multi-row statement per table instead of one per row.

/** How many rows go into a single INSERT. Postgres caps a statement at 65535
 *  bind parameters; the widest table here is ~20 columns, so 500 rows is an
 *  order of magnitude inside the limit and still collapses any realistic org
 *  into one statement per table. */
const WRITE_CHUNK = 500;

function chunked<T>(rows: T[], size = WRITE_CHUNK): T[][] {
  if (rows.length <= size) return rows.length ? [rows] : [];
  const out: T[][] = [];
  for (let i = 0; i < rows.length; i += size) out.push(rows.slice(i, i + size));
  return out;
}

/** `SET col = excluded.col` for every named column.
 *
 *  A one-row upsert can name the literal value in its SET clause; a multi-row
 *  one cannot, because all the rows share a single SET and each must take its
 *  value from the row Postgres was about to insert. `excluded` is that row. */
function excludedSet<K extends string>(table: PgTable, keys: readonly K[]) {
  const cols = getTableColumns(table) as Record<string, { name: string }>;
  const set: Record<string, SQL> = {};
  for (const k of keys) set[k] = sql.raw(`excluded."${cols[k].name}"`);
  return set;
}

/** Recursively key-sorted, so two jsonb blobs that differ only in key order
 *  (Postgres does not preserve the order it was given) compare equal. */
function canonical(v: unknown): unknown {
  if (Array.isArray(v)) return v.map(canonical);
  if (v && typeof v === "object") {
    const src = v as Record<string, unknown>;
    const out: Record<string, unknown> = {};
    for (const k of Object.keys(src).sort()) out[k] = canonical(src[k]);
    return out;
  }
  return v;
}

function sameValue(a: unknown, b: unknown): boolean {
  if (a === b) return true;
  if (a instanceof Date || b instanceof Date) {
    return (
      a instanceof Date &&
      b instanceof Date &&
      a.getTime() === b.getTime()
    );
  }
  if (a === null || b === null || a === undefined || b === undefined) return false;
  if (typeof a === "object" && typeof b === "object") {
    return JSON.stringify(canonical(a)) === JSON.stringify(canonical(b));
  }
  return false;
}

/** True when every mirrored column of `next` already holds that value locally.
 *  A missing local row is never "unchanged" — it has to be inserted. */
function unchanged<K extends string>(
  existing: Record<string, unknown> | undefined,
  next: Record<string, unknown>,
  keys: readonly K[]
): boolean {
  if (!existing) return false;
  return keys.every((k) => sameValue(existing[k], next[k]));
}

// The mirrored columns per table: exactly the ones the upsert's SET clause
// writes, which is what makes "unchanged" mean "this upsert would be a no-op".
// Columns the mirror owns locally (slug, created_at, byo_vpn, name_auto…) are
// deliberately absent from both.
const SERVER_KEYS = [
  "name",
  "type",
  "source",
  "provider",
  "region",
  "status",
  "agentVersion",
  "ip",
  "meshIp",
  "cpu",
  "memGb",
  // Detected facts and the gate's verdict move — a driver appears, a disk
  // grows, a type is changed — so the mirror has to follow them like every
  // other mutable column here, or it freezes at whatever the host looked like
  // the first time it was synced.
  "facts",
  "incompatibleReasons",
  // A decommission starts and ends on the control plane's clock, so a mirror
  // that froze this would show a stale "Decommissioning" pill — or miss one
  // entirely — on every page that reads the mirror.
  "decommissionStartedAt",
  "decommissionPurgeVolumes",
] as const;
const PROJECT_KEYS = ["name", "slug", "description"] as const;
const ENVIRONMENT_KEYS = ["name", "projectId", "production"] as const;
const RESOURCE_KEYS = [
  "projectId",
  "environmentId",
  "serverId",
  "name",
  "kind",
  "status",
  "repo",
  "domain",
  "ephemeral",
  "version",
  "lastDeployAt",
] as const;
const DEPLOYMENT_KEYS = ["status", "durationSec", "startedAt"] as const;

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

  // The deployment rows to mirror: the CP's recent feed plus the latest per
  // resource, restricted to resources that exist locally (FK) and deduped by id
  // (SIGMA-161). Computed here, before the reads, so the diff below can load
  // exactly these ids' mirror rows in the same round of selects.
  const cpResourceIds = new Set(cpResources.map((r) => r.id));
  const deployRows = new Map<string, (typeof cpDeploys.recent)[number]>();
  for (const d of [...cpDeploys.recent, ...cpDeploys.latest]) {
    if (cpResourceIds.has(d.resourceId)) deployRows.set(d.id, d);
  }
  const deployIds = [...deployRows.keys()];
  const cpEnvIds = cpEnvs.map((e) => e.id);

  // Existing mirror rows, org-scoped: locally-kept fields (slug, demo status)
  // survive the upsert, and the id sets feed the tombstone diffs. Scoping by
  // org here is what makes the deletes below safe.
  //
  // Every mirrored column is selected, not just the id: the diff below decides
  // whether a row needs writing at all, and it can only do that against the
  // values the mirror currently holds (SIGMA-327).
  const [
    localServers,
    localProjects,
    localEnvs,
    localResources,
    localDeploys,
    localAttachments,
  ] = await Promise.all([
      db.select().from(s.servers).where(eq(s.servers.orgId, orgId)),
      db.select().from(s.projects).where(eq(s.projects.orgId, orgId)),
      db
        .select({
          id: s.environments.id,
          projectId: s.environments.projectId,
          name: s.environments.name,
          production: s.environments.production,
        })
        .from(s.environments)
        .innerJoin(s.projects, eq(s.environments.projectId, s.projects.id))
        .where(eq(s.projects.orgId, orgId)),
      db
        .select({
          id: s.resources.id,
          projectId: s.resources.projectId,
          environmentId: s.resources.environmentId,
          serverId: s.resources.serverId,
          name: s.resources.name,
          kind: s.resources.kind,
          status: s.resources.status,
          repo: s.resources.repo,
          domain: s.resources.domain,
          ephemeral: s.resources.ephemeral,
          version: s.resources.version,
          lastDeployAt: s.resources.lastDeployAt,
        })
        .from(s.resources)
        .innerJoin(s.projects, eq(s.resources.projectId, s.projects.id))
        .where(eq(s.projects.orgId, orgId)),
      deployIds.length
        ? db
            .select({
              id: s.deployments.id,
              status: s.deployments.status,
              durationSec: s.deployments.durationSec,
              startedAt: s.deployments.startedAt,
            })
            .from(s.deployments)
            .where(inArray(s.deployments.id, deployIds))
        : [],
      cpEnvIds.length
        ? db
            .select({
              environmentId: s.envServers.environmentId,
              serverId: s.envServers.serverId,
            })
            .from(s.envServers)
            .where(inArray(s.envServers.environmentId, cpEnvIds))
        : [],
    ]);

  // 1. Servers first — resources and env_servers FK onto them.
  const serversById = new Map(localServers.map((r) => [r.id, r]));
  const serverRows = cpServers
    .map(cpServerToRow)
    .filter((row) => !unchanged(serversById.get(row.id), row, SERVER_KEYS));
  for (const batch of chunked(serverRows)) {
    await db
      .insert(s.servers)
      .values(batch)
      .onConflictDoUpdate({
        target: s.servers.id,
        set: excludedSet(s.servers, SERVER_KEYS),
      });
  }

  // 2. Projects (delete cascades their envs/resources).
  const projectsById = new Map(localProjects.map((p) => [p.id, p]));
  const projectRows = cpProjects
    .map((p) => projectMirrorRow(p, projectsById.get(p.id)))
    .filter((row) => !unchanged(projectsById.get(row.id), row, PROJECT_KEYS));
  for (const batch of chunked(projectRows)) {
    await db
      .insert(s.projects)
      .values(batch)
      .onConflictDoUpdate({
        target: s.projects.id,
        set: excludedSet(s.projects, PROJECT_KEYS),
      });
  }

  // 3. Environments — this is what makes CP-created preview envs (pr-<n>)
  // visible in the project pages.
  const envsById = new Map(localEnvs.map((e) => [e.id, e]));
  const envRows = cpEnvs
    .map(environmentMirrorRow)
    .filter((row) => !unchanged(envsById.get(row.id), row, ENVIRONMENT_KEYS));
  for (const batch of chunked(envRows)) {
    await db
      .insert(s.environments)
      .values(batch)
      .onConflictDoUpdate({
        target: s.environments.id,
        set: excludedSet(s.environments, ENVIRONMENT_KEYS),
      });
  }

  // 4. Resources. The latest CP deployment per resource feeds version and
  // lastDeployAt (SIGMA-161 — both previously froze at resource creation).
  const latestByResource = new Map(cpDeploys.latest.map((d) => [d.resourceId, d]));
  const resourcesById = new Map(localResources.map((r) => [r.id, r]));
  const resourceRows = cpResources
    .map((res) =>
      resourceMirrorRow(res, resourcesById.get(res.id), latestByResource.get(res.id))
    )
    .filter((row) => !unchanged(resourcesById.get(row.id), row, RESOURCE_KEYS));
  for (const batch of chunked(resourceRows)) {
    await db
      .insert(s.resources)
      .values(batch)
      .onConflictDoUpdate({
        target: s.resources.id,
        set: excludedSet(s.resources, RESOURCE_KEYS),
      });
  }

  // 4b. Deployments — mirror the CP's recent + latest rows into the local
  // deployments table every existing surface already reads (activity feed,
  // Active deploys stat, per-resource latestDeploy).
  const deploysById = new Map(localDeploys.map((d) => [d.id, d]));
  const deploymentRows = [...deployRows.values()]
    .map((d) => ({
      id: d.id,
      resourceId: d.resourceId,
      sha: d.gitSha ?? "",
      status: localDeployStatus(d.status),
      author: d.createdBy ?? "",
      durationSec: d.durationSeconds ?? 0,
      startedAt: new Date(d.startedAt ?? d.createdAt),
    }))
    .filter((row) => !unchanged(deploysById.get(row.id), row, DEPLOYMENT_KEYS));
  for (const batch of chunked(deploymentRows)) {
    await db
      .insert(s.deployments)
      .values(batch)
      .onConflictDoUpdate({
        target: s.deployments.id,
        set: excludedSet(s.deployments, DEPLOYMENT_KEYS),
      });
  }

  // 5. Env↔server attachments, reconciled as one set rather than replaced one
  // environment at a time. The old shape issued an insert per attachment and a
  // DELETE per environment — 60 deletes for 60 environments, every sync, even
  // when nothing had moved.
  const pairKey = (envId: string, serverId: string) => `${envId} ${serverId}`;
  const localPairs = new Set(
    localAttachments.map((a) => pairKey(a.environmentId, a.serverId))
  );
  const cpPairs = new Set<string>();
  const attachRows: { environmentId: string; serverId: string }[] = [];
  for (const { envId, serverIds } of envAttachments) {
    for (const serverId of serverIds) {
      cpPairs.add(pairKey(envId, serverId));
      if (!localPairs.has(pairKey(envId, serverId))) {
        attachRows.push({ environmentId: envId, serverId });
      }
    }
  }
  for (const batch of chunked(attachRows)) {
    await db.insert(s.envServers).values(batch).onConflictDoNothing();
  }
  const detached = localAttachments.filter(
    (a) => !cpPairs.has(pairKey(a.environmentId, a.serverId))
  );
  if (detached.length) {
    // One statement for the whole org: env_servers has a composite key, so the
    // predicate is an OR of (environment_id, server_id) pairs rather than an IN.
    await db
      .delete(s.envServers)
      .where(
        or(
          ...detached.map((a) =>
            and(
              eq(s.envServers.environmentId, a.environmentId),
              eq(s.envServers.serverId, a.serverId)
            )
          )
        )
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
