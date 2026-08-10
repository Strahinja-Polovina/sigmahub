// How much deployment history a page is allowed to load (SIGMA-329).
//
// getResourceDetail is a server component's data source: whatever it returns is
// serialised into the RSC payload and shipped to the browser. Its deployments
// query selected every row for the resource — ordered, but with no limit — to
// feed a panel that shows a couple of dozen releases. An app deployed on every
// push accumulates thousands of rows, so time-to-first-byte and payload size
// for its resource page grew without bound over the resource's life, and the
// two modes disagreed about how much history a resource even has: the control
// plane caps release history at 20 (ListDeployments, cp/internal/store/
// deployments.go), while demo mode returned all 4,000.
//
// getDeployments had the same unbounded select and no ORDER BY at all, so the
// order of "recent deployments" was whatever Postgres felt like returning.
//
// These tests run against the real migrated schema (PGlite, see
// @/server/testing/demo-db) rather than a mock, because the thing under test is
// the SQL — a stubbed drizzle would happily "pass" a query with no LIMIT in it.

import { beforeAll, beforeEach, describe, expect, it, vi } from "vitest";

import * as s from "@/server/db/schema";
import { FIXTURE, seedDemoFixture, type DemoDb } from "@/server/testing/demo-db";

vi.mock("@/server/db", async () => {
  const { createDemoDb } = await import("@/server/testing/demo-db");
  return { db: await createDemoDb() };
});
// queries.ts reaches the control plane for the servers list; nothing in this
// file's queries goes near it, and cpEnabled() false keeps it that way.
vi.mock("@/server/cp", () => ({
  cpEnabled: () => false,
  cpListServers: async () => [],
  cpServerToRow: (x: unknown) => x,
}));
vi.mock("@/server/cp-sync", () => ({ reportCpFailure: async () => {} }));

const RESOURCE_ID = "res_busy";
const SEEDED = 100;

let db: DemoDb;
let getResourceDetail: typeof import("./queries").getResourceDetail;
let getDeployments: typeof import("./queries").getDeployments;

beforeAll(async () => {
  db = (await import("@/server/db")).db as unknown as DemoDb;
  ({ getResourceDetail, getDeployments } = await import("./queries"));
});

beforeEach(async () => {
  await db.delete(s.deployments);
  await db.delete(s.resources);
  await db.delete(s.environments);
  await db.delete(s.projects);
  await db.delete(s.servers);
  await db.delete(s.orgs);
  await seedDemoFixture(db);
  await db.insert(s.resources).values({
    id: RESOURCE_ID,
    projectId: FIXTURE.projectId,
    environmentId: FIXTURE.prodEnvId,
    serverId: FIXTURE.dbHostId,
    name: "checkout",
    kind: "app",
  });
  // A year of pushes. Row i started i minutes after the epoch below, so the
  // newest is deploy-99 and any correct "newest first" answer starts there.
  const base = Date.UTC(2026, 0, 1, 0, 0, 0);
  await db.insert(s.deployments).values(
    Array.from({ length: SEEDED }, (_, i) => ({
      id: `dep_${String(i).padStart(3, "0")}`,
      resourceId: RESOURCE_ID,
      sha: `sha${i}`,
      status: "success",
      startedAt: new Date(base + i * 60_000),
    }))
  );
});

describe("getResourceDetail", () => {
  it("returns at most 25 deployments, newest first", async () => {
    const detail = await getResourceDetail(RESOURCE_ID);
    expect(detail).toBeDefined();

    // The bound: the page renders a couple of dozen releases, so a resource
    // with 100 (or 4,000) must not serialise all of them into the RSC payload.
    expect(detail!.deployments.length).toBeLessThanOrEqual(25);
    expect(detail!.deployments.length).toBe(25);

    // ...and the 25 it keeps are the newest 25, in order. A LIMIT without a
    // matching ORDER BY would be worse than none: it would silently drop the
    // deploys the panel exists to show.
    expect(detail!.deployments.map((d) => d.id)).toEqual(
      Array.from({ length: 25 }, (_, i) => `dep_${String(SEEDED - 1 - i).padStart(3, "0")}`)
    );
  });
});

describe("getDeployments", () => {
  it("is ordered newest first and bounded by default", async () => {
    const rows = await getDeployments(RESOURCE_ID);
    expect(rows.length).toBe(25);
    expect(rows[0].id).toBe("dep_099");
    expect(rows.at(-1)!.id).toBe("dep_075");
  });

  it("honours an explicit limit", async () => {
    const rows = await getDeployments(RESOURCE_ID, 5);
    expect(rows.map((d) => d.id)).toEqual([
      "dep_099",
      "dep_098",
      "dep_097",
      "dep_096",
      "dep_095",
    ]);
  });
});
