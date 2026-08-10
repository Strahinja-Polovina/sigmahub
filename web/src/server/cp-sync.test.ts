/**
 * Write-shape tests for the CP → local mirror reconciler.
 *
 * syncOrgMirror is awaited by the dashboard layout on every render (throttled
 * to one pass per org per 30 s, with an 8 s bail-out), so what it costs in
 * round-trips is what a user waits for on their first navigation in each
 * window. These tests assert on the STATEMENTS it issues, not only on the rows
 * it leaves behind: a per-row upsert loop and a batched upsert converge on the
 * same mirror, so nothing else can tell them apart (SIGMA-327).
 */
import { beforeEach, describe, expect, it, vi } from "vitest";

const harness = vi.hoisted(() => ({
  db: null as unknown as import("./testing/demo-db").DemoDb,
  sql: [] as string[],
}));

vi.mock("./db", async () => {
  const { createDemoDb } = await import("./testing/demo-db");
  harness.db = await createDemoDb({ onQuery: (q) => harness.sql.push(q) });
  return { db: harness.db };
});

const ORG = "org_sync";
const PROJECTS = 3;
const ENVS_PER_PROJECT = 2;
const SERVERS = 5;
const RESOURCES = 20;

const projId = (p: number) => `proj_${p}`;
const envId = (p: number, e: number) => `env_${p}_${e}`;
const srvId = (i: number) => `srv_${i}`;
const resId = (i: number) => `res_${i}`;

const AT = "2026-01-01T00:00:00.000Z";

const cpServers = Array.from({ length: SERVERS }, (_, i) => ({
  id: srvId(i),
  orgId: ORG,
  name: `srv-${i}`,
  type: "application",
  provider: "hetzner",
  region: "fsn1",
  status: "running",
  agentVersion: "1.2.3",
  // A non-empty facts blob on purpose: it is jsonb, and Postgres does not
  // preserve key order, so an unchanged-row diff that compares it naively
  // would report a change on every sync.
  facts: { numCpu: 4, memTotalMb: 8192, arch: "x86_64" },
  meshIp: `10.8.0.${i + 1}`,
  endpoint: `203.0.113.${i + 1}:51820`,
  pubkey: "k",
  lastSeenAt: AT,
  createdAt: AT,
  incompatibleReasons: [],
}));

const cpProjects = Array.from({ length: PROJECTS }, (_, p) => ({
  id: projId(p),
  orgId: ORG,
  name: `Project ${p}`,
  description: `desc ${p}`,
  createdBy: "u1",
  createdAt: AT,
}));

const cpEnvsByProject = new Map(
  cpProjects.map((p, i) => [
    p.id,
    Array.from({ length: ENVS_PER_PROJECT }, (_, e) => ({
      id: envId(i, e),
      orgId: ORG,
      projectId: p.id,
      name: e === 0 ? "production" : "staging",
      production: e === 0,
      createdAt: AT,
    })),
  ])
);

const cpResources = Array.from({ length: RESOURCES }, (_, i) => ({
  id: resId(i),
  orgId: ORG,
  projectId: projId(i % PROJECTS),
  environmentId: envId(i % PROJECTS, i % ENVS_PER_PROJECT),
  serverId: srvId(i % SERVERS),
  name: `res-${i}`,
  kind: "app",
  spec: { repo: `acme/app-${i}`, domain: `app-${i}.example.com` },
  status: { state: "running" },
  ephemeral: false,
  createdAt: AT,
  updatedAt: AT,
}));

const latest = cpResources.map((r, i) => ({
  id: `dep_${i}_latest`,
  orgId: ORG,
  resourceId: r.id,
  trigger: "git",
  gitSha: `abcdef01234${i}`,
  status: "success",
  durationSeconds: 42,
  createdBy: "u1",
  createdAt: AT,
  startedAt: AT,
}));
const recent = latest.slice(0, 10);

const attachments = new Map<string, string[]>();
for (const envs of cpEnvsByProject.values()) {
  attachments.set(envs[0].id, [srvId(0), srvId(1)]);
  attachments.set(envs[1].id, [srvId(2)]);
}

const cpListServers = vi.fn(async () => cpServers);
const cpListProjects = vi.fn(async () => cpProjects);
const cpListResources = vi.fn(async () => cpResources);
const cpListOrgDeployments = vi.fn(async () => ({ recent, latest }));
const cpListEnvironments = vi.fn(
  async (_orgId: string, projectId: string) => cpEnvsByProject.get(projectId) ?? []
);
const cpEnvServerIds = vi.fn(
  async (_orgId: string, id: string) => attachments.get(id) ?? []
);

vi.mock("./cp", async () => {
  const actual = await vi.importActual<typeof import("./cp")>("./cp");
  return {
    cpEnabled: () => true,
    cpServerToRow: actual.cpServerToRow,
    cpListServers: () => cpListServers(),
    cpListProjects: () => cpListProjects(),
    cpListResources: () => cpListResources(),
    cpListOrgDeployments: () => cpListOrgDeployments(),
    cpListEnvironments: (o: string, p: string) => cpListEnvironments(o, p),
    cpEnvServerIds: (o: string, e: string) => cpEnvServerIds(o, e),
  };
});

import { syncOrgMirror } from "./cp-sync";
import * as schema from "./db/schema";

/** Statements that change rows. Reads are not what this ticket is about. */
function writes(): string[] {
  return harness.sql.filter((q) => /^\s*(insert|update|delete)\b/i.test(q));
}

/** The table a write statement targets, for the per-table batching assertion. */
function target(stmt: string): string {
  const m = /^\s*(?:insert\s+into|update|delete\s+from)\s+"([^"]+)"/i.exec(stmt);
  return m ? m[1] : stmt.slice(0, 40);
}

beforeEach(() => {
  harness.sql.length = 0;
});

describe("syncOrgMirror", () => {
  it("writes are batched per table", async () => {
    await harness.db
      .insert(schema.orgs)
      .values({ id: ORG, name: "Sync", slug: "sync" })
      .onConflictDoNothing();
    harness.sql.length = 0;

    await syncOrgMirror(ORG);

    const perTable = new Map<string, number>();
    for (const w of writes()) perTable.set(target(w), (perTable.get(target(w)) ?? 0) + 1);
    // servers, projects, environments, resources, deployments, env_servers —
    // one statement each. A per-row loop makes this 5, 3, 6, 20, 20 and 9.
    for (const [table, n] of perTable) {
      expect(`${table}=${n}`).toBe(`${table}=1`);
    }
    expect(writes().length).toBeLessThanOrEqual(8);

    // The mirror is still the mirror: batching must not lose rows.
    expect(await harness.db.$count(schema.servers)).toBe(SERVERS);
    expect(await harness.db.$count(schema.projects)).toBe(PROJECTS);
    expect(await harness.db.$count(schema.environments)).toBe(
      PROJECTS * ENVS_PER_PROJECT
    );
    expect(await harness.db.$count(schema.resources)).toBe(RESOURCES);
    expect(await harness.db.$count(schema.deployments)).toBe(RESOURCES);
    expect(await harness.db.$count(schema.envServers)).toBe(PROJECTS * 3);
  });

  it("a sync of an unchanged org issues no writes", async () => {
    // First pass populates; the second has nothing to say.
    await syncOrgMirror(ORG);
    harness.sql.length = 0;
    await syncOrgMirror(ORG);
    expect(writes()).toEqual([]);
  });

  it("a changed row is still written, and only that row", async () => {
    await syncOrgMirror(ORG);
    cpServers[2].status = "degraded";
    cpResources[7].name = "res-7-renamed";
    harness.sql.length = 0;
    try {
      await syncOrgMirror(ORG);
      expect(writes().map(target).sort()).toEqual(["resources", "servers"]);
    } finally {
      cpServers[2].status = "running";
      cpResources[7].name = "res-7";
    }

    const rows = await harness.db.select().from(schema.servers);
    expect(rows.find((r) => r.id === srvId(2))?.status).toBe("degraded");
    const res = await harness.db.select().from(schema.resources);
    expect(res.find((r) => r.id === resId(7))?.name).toBe("res-7-renamed");
  });

  it("a detached server is removed from its environment", async () => {
    await syncOrgMirror(ORG);
    const firstEnv = cpEnvsByProject.get(projId(0))![0].id;
    attachments.set(firstEnv, [srvId(0)]);
    harness.sql.length = 0;
    try {
      await syncOrgMirror(ORG);
      const rows = await harness.db.select().from(schema.envServers);
      expect(
        rows.filter((r) => r.environmentId === firstEnv).map((r) => r.serverId)
      ).toEqual([srvId(0)]);
      // One bulk delete, not one per environment.
      expect(writes().filter((w) => /^\s*delete/i.test(w))).toHaveLength(1);
    } finally {
      attachments.set(firstEnv, [srvId(0), srvId(1)]);
    }
  });

  it("tombstones rows the control plane no longer owns", async () => {
    await syncOrgMirror(ORG);
    const dropped = cpResources.pop()!;
    try {
      await syncOrgMirror(ORG);
      const res = await harness.db.select().from(schema.resources);
      expect(res.find((r) => r.id === dropped.id)).toBeUndefined();
      expect(res).toHaveLength(RESOURCES - 1);
    } finally {
      cpResources.push(dropped);
    }
    await syncOrgMirror(ORG);
    expect(await harness.db.$count(schema.resources)).toBe(RESOURCES);
  });
});
