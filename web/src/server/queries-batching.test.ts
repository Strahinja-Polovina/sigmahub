// SIGMA-326 lives in its own file rather than in queries.test.ts.
//
// SIGMA-329 created a queries.test.ts of its own in the same wave, and the two
// harnesses cannot share a module: this one mocks "./db" with a factory that
// builds its schema seed inline, that one mocks "@/server/db" through
// seedDemoFixture. Same module, two different factories — merged into one file
// they would be duplicate vi.mock calls where the last registration silently
// wins and half the suite would assert against the wrong fixture.
/**
 * Access-pattern tests for the dashboard read models.
 *
 * These assert on how many statements a read model issues, not only on what it
 * returns. An N+1 loop and a batched join return identical rows, so a test that
 * only checks the answer cannot tell them apart — and every one of these read
 * models is awaited on a `no-store` page, so the statement count IS the
 * user-visible behaviour (SIGMA-326).
 */
import { beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { eq } from "drizzle-orm";

const harness = vi.hoisted(() => ({
  db: null as unknown as import("./testing/demo-db").DemoDb,
  sql: [] as string[],
}));

vi.mock("./db", async () => {
  const { createDemoDb } = await import("./testing/demo-db");
  harness.db = await createDemoDb({ onQuery: (q) => harness.sql.push(q) });
  return { db: harness.db };
});

// The control-plane client is not what is under test here; demo mode (no CP)
// is the branch that reads the local mirror.
const cpEnabled = vi.fn(() => false);
const cpListServers = vi.fn(async () => [] as unknown[]);
/** What the control plane would answer for each org's ?count=1. Distinct
 *  numbers so a count attributed to the wrong org is visible. */
const CP_COUNTS: Record<string, number> = { org_scale: 7, org_other: 4 };
const cpServerCount = vi.fn(async (orgId: string) => CP_COUNTS[orgId] ?? 0);
vi.mock("./cp", () => ({
  cpEnabled: () => cpEnabled(),
  cpListServers: () => cpListServers(),
  cpServerCount: (orgId: string) => cpServerCount(orgId),
  cpServerToRow: (r: unknown) => r,
}));
vi.mock("./cp-sync", () => ({ reportCpFailure: () => {} }));

import * as schema from "./db/schema";
import {
  getOrgResources,
  getProjectSummaries,
  getServerCounts,
  getServersWithCounts,
} from "./queries";

const ORG = "org_scale";
/** Ten projects and fifty resources: enough that a per-row loop and a batched
 *  query differ by an order of magnitude in statement count. */
const PROJECTS = 10;
const SERVERS = 5;
const N = 50;
const proj = (i: number) => `proj_${i}`;
const env = (i: number) => `env_${i}`;

beforeAll(async () => {
  const db = harness.db;
  await db.insert(schema.orgs).values({ id: ORG, name: "Scale", slug: "scale" });
  await db.insert(schema.projects).values(
    Array.from({ length: PROJECTS }, (_, i) => ({
      id: proj(i),
      orgId: ORG,
      name: `Project ${i}`,
      slug: `project-${i}`,
    }))
  );
  await db.insert(schema.environments).values(
    Array.from({ length: PROJECTS }, (_, i) => ({
      id: env(i),
      projectId: proj(i),
      name: "production",
    }))
  );
  await db.insert(schema.servers).values(
    Array.from({ length: SERVERS }, (_, i) => ({
      id: `srv_${i}`,
      orgId: ORG,
      name: `srv-${i}`,
      type: "application",
      provider: "hetzner",
      region: "fsn1",
      status: "running",
    }))
  );
  // Every server is attached to the first project's environment only, so the
  // summaries have to differ per project rather than all reading the same set.
  await db.insert(schema.envServers).values(
    Array.from({ length: SERVERS }, (_, i) => ({
      environmentId: env(0),
      serverId: `srv_${i}`,
    }))
  );
  await db.insert(schema.resources).values(
    Array.from({ length: N }, (_, i) => ({
      id: `res_${i}`,
      projectId: proj(i % PROJECTS),
      environmentId: env(i % PROJECTS),
      serverId: `srv_${i % SERVERS}`,
      name: `res-${i}`,
      kind: "app",
      status: Math.floor(i / PROJECTS) < 2 ? "running" : "stopped",
    }))
  );
  // Two deployments per resource, so "the latest one" is a real choice: a
  // batched query that picks the wrong row per resource fails on content too.
  await db.insert(schema.deployments).values(
    Array.from({ length: N }, (_, i) => [
      {
        id: `dep_${i}_old`,
        resourceId: `res_${i}`,
        sha: `old${i}`,
        status: "success",
        author: "old",
        startedAt: new Date(Date.UTC(2026, 0, 1, 0, i)),
      },
      {
        id: `dep_${i}_new`,
        resourceId: `res_${i}`,
        sha: `new${i}`,
        status: "running",
        author: "new",
        startedAt: new Date(Date.UTC(2026, 0, 2, 0, i)),
      },
    ]).flat()
  );
});

beforeEach(() => {
  harness.sql.length = 0;
  vi.clearAllMocks();
  cpEnabled.mockReturnValue(false);
});

describe("getOrgResources", () => {
  it("issues a bounded number of queries", async () => {
    const rows = await getOrgResources(ORG);
    expect(rows).toHaveLength(N);
    // The resource join, plus one batched latest-deployment query. Anything
    // that scales with the resource count is the N+1 this test exists to catch.
    expect(harness.sql.length).toBeLessThanOrEqual(3);
  });

  it("still carries each resource's most recent deployment", async () => {
    const rows = await getOrgResources(ORG);
    const byId = new Map(rows.map((r) => [r.id, r]));
    for (let i = 0; i < N; i++) {
      const row = byId.get(`res_${i}`)!;
      expect(row.latestDeploy?.id).toBe(`dep_${i}_new`);
      expect(row.projectName).toBe(`Project ${i % PROJECTS}`);
      expect(row.envName).toBe("production");
    }
  });

  it("a resource with no deployment carries null", async () => {
    const db = harness.db;
    await db.insert(schema.resources).values({
      id: "res_never",
      projectId: proj(0),
      environmentId: env(0),
      name: "never",
      kind: "app",
      status: "provisioning",
    });
    try {
      const rows = await getOrgResources(ORG);
      expect(rows.find((r) => r.id === "res_never")?.latestDeploy).toBeNull();
    } finally {
      await db.delete(schema.resources).where(eq(schema.resources.id, "res_never"));
    }
  });

  it("honours project scoping", async () => {
    expect(await getOrgResources(ORG, new Set<string>())).toEqual([]);
    expect(await getOrgResources(ORG, new Set([proj(0)]))).toHaveLength(N / PROJECTS);
  });
});

describe("getServersWithCounts", () => {
  it("issues a bounded number of queries", async () => {
    const rows = await getServersWithCounts(ORG);
    expect(rows).toHaveLength(SERVERS);
    expect(harness.sql.length).toBeLessThanOrEqual(2);
  });

  it("counts the resources scheduled on each server", async () => {
    const rows = await getServersWithCounts(ORG);
    expect(rows).toHaveLength(SERVERS);
    for (const row of rows) expect(row.resourceCount).toBe(N / SERVERS);
  });
});

describe("getServerCounts", () => {
  it("does not fetch full server projections", async () => {
    cpEnabled.mockReturnValue(true);
    const counts = await getServerCounts([ORG, "org_other"]);

    // SIGMA-335: the org switcher renders one integer per org. Asking
    // GET /v1/orgs/{id}/servers for it makes the control plane build the whole
    // dashboard projection — every column, the facts jsonb blob and a
    // correlated readiness subquery per row — and serialise it so the web can
    // call .length on it. The layout does that on EVERY render, for every org
    // the user belongs to, with no caching.
    expect(cpListServers).not.toHaveBeenCalled();
    expect(cpServerCount).toHaveBeenCalledTimes(2);
    expect(counts).toEqual({ [ORG]: 7, org_other: 4 });
  });

  it("a control-plane hiccup shows zero rather than taking down the switcher", async () => {
    cpEnabled.mockReturnValue(true);
    cpServerCount.mockRejectedValueOnce(new Error("connect ECONNREFUSED"));
    expect(await getServerCounts([ORG])).toEqual({ [ORG]: 0 });
  });

  it("demo mode counts the mirror rows in one query", async () => {
    const counts = await getServerCounts([ORG, "org_other"]);
    expect(counts).toEqual({ [ORG]: SERVERS, org_other: 0 });
    expect(harness.sql).toHaveLength(1);
  });
});

describe("getProjectSummaries", () => {
  it("issues a bounded number of queries", async () => {
    const rows = await getProjectSummaries(ORG);
    expect(rows).toHaveLength(PROJECTS);
    expect(harness.sql.length).toBeLessThanOrEqual(5);
  });

  it("reports the same counts the per-project queries did", async () => {
    const rows = await getProjectSummaries(ORG);
    const byId = new Map(rows.map((r) => [r.project.id, r]));
    for (let i = 0; i < PROJECTS; i++) {
      const summary = byId.get(proj(i))!;
      expect(summary.envCount).toBe(1);
      // Only the first project's environment has servers attached.
      expect(summary.serverCount).toBe(i === 0 ? SERVERS : 0);
      expect(summary.resourceCount).toBe(N / PROJECTS);
      expect(summary.statusCounts).toEqual({ running: 2, stopped: 3 });
      expect(summary.resources).toHaveLength(N / PROJECTS);
      expect(summary.resources[0].envName).toBe("production");
    }
  });
});
