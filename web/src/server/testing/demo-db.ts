/**
 * A real database for the demo-mode server actions to be tested against.
 *
 * The demo branch of a server action is a second implementation of the
 * product's rules — one cluster per environment, the kinds a cluster refuses,
 * the control-plane node that cannot be drained — and none of it was covered by
 * anything, because the actions are unimportable: they open with "use server"
 * and pull in next/cache, better-auth and the control-plane client, and the
 * decisions themselves are SQL. A reviewer deleted three of those refusals and
 * the suite stayed green.
 *
 * So the test gets the same database the demo runs on. PGlite is already the
 * demo's engine (see ../db), it runs in-process with no server to start, and
 * the migrations applied here are the ones shipped in drizzle/ — which means a
 * column that only exists in schema.ts fails these tests rather than production.
 * Everything else the actions touch is mocked per test file: identity, the
 * audit log, revalidatePath and the CP client, none of which the demo branch
 * consults.
 *
 * Test files wire it in with:
 *
 *     vi.mock("@/server/db", async () => ({ db: await createDemoDb() }));
 */

import { PGlite } from "@electric-sql/pglite";
import { drizzle } from "drizzle-orm/pglite";
import { migrate } from "drizzle-orm/pglite/migrator";
import type { NodePgDatabase } from "drizzle-orm/node-postgres";
import * as schema from "../db/schema";
import * as authSchema from "../db/auth-schema";
import { SERVER_STATUS } from "@/lib/server-compat";

const fullSchema = { ...schema, ...authSchema };

/** The type the app's own `db` export carries, so a mocked module is a drop-in
 *  for the real one and a query the actions can make is a query the test can. */
export type DemoDb = NodePgDatabase<typeof fullSchema>;

/** Options for createDemoDb.
 *
 *  `onQuery` receives every statement drizzle sends, in order. It exists so a
 *  test can assert on the SHAPE of a read model's access pattern rather than
 *  only on its answer: an N+1 loop and a batched join return identical rows, so
 *  the only way a regression test can tell them apart is by counting the
 *  statements (SIGMA-326). Migrations run before the hook is armed, so a
 *  counter only ever sees what the code under test issued. */
export type DemoDbOptions = {
  onQuery?: (sql: string, params: unknown[]) => void;
};

/** A migrated, empty database. In memory: a directory would persist between
 *  runs and make the suite's answers depend on what the last one wrote. */
export async function createDemoDb(opts: DemoDbOptions = {}): Promise<DemoDb> {
  let armed = false;
  const pglite = drizzle(new PGlite(), {
    schema: fullSchema,
    logger: opts.onQuery
      ? { logQuery: (query, params) => armed && opts.onQuery!(query, params) }
      : undefined,
  });
  await migrate(pglite, { migrationsFolder: "drizzle" });
  armed = true;
  // The same widening ../db does: the two drizzle instances are structurally
  // identical at every call site, and the node-postgres type is what the app
  // exports.
  return pglite as unknown as DemoDb;
}

/**
 * The fleet every demo-mode test starts from: one org with a project, two
 * environments and a handful of hosts, plus a second org to point cross-tenant
 * ids at.
 *
 * Named rather than generated, because the refusals under test are about
 * particular hosts — one that has not checked in yet, one that belongs to
 * somebody else — and a fixture whose ids mean nothing makes the failure
 * message mean nothing too.
 */
export const FIXTURE = {
  orgId: "org_acme",
  rivalOrgId: "org_rival",
  projectId: "proj_shop",
  prodEnvId: "env_shop_prod",
  stagingEnvId: "env_shop_staging",
  /** Cluster-capable hosts, all checked in and running. */
  k8sHostIds: ["srv_k8s_1", "srv_k8s_2", "srv_k8s_3"] as const,
  /** A database host, for the managed kinds that run on their own server. */
  dbHostId: "srv_db_1",
  /** An object-storage host, where an S3 resource is provisioned. */
  storageHostId: "srv_store_1",
  /** Connected to the org, but the agent has never called home. */
  unconnectedHostId: "srv_pending",
  /** Enrolled under a type its reported facts contradict, so the gate refused
   *  it — a host the availability matrix accepts on paper and not in fact. */
  incompatibleHostId: "srv_wrong_type",
  /** A perfectly good host in somebody else's organization. */
  rivalHostId: "srv_rival_1",
} as const;

/** Insert FIXTURE into a database created by createDemoDb. */
export async function seedDemoFixture(db: DemoDb): Promise<void> {
  await db.insert(schema.orgs).values([
    { id: FIXTURE.orgId, name: "Acme", slug: "acme" },
    { id: FIXTURE.rivalOrgId, name: "Rival", slug: "rival" },
  ]);
  await db.insert(schema.projects).values({
    id: FIXTURE.projectId,
    orgId: FIXTURE.orgId,
    name: "Shop",
    slug: "shop",
  });
  await db.insert(schema.environments).values([
    { id: FIXTURE.prodEnvId, projectId: FIXTURE.projectId, name: "production", production: true },
    { id: FIXTURE.stagingEnvId, projectId: FIXTURE.projectId, name: "staging" },
  ]);
  await db.insert(schema.servers).values([
    ...FIXTURE.k8sHostIds.map((id, i) => ({
      id,
      orgId: FIXTURE.orgId,
      name: `k8s-${i + 1}`,
      type: "k8s",
      provider: "hetzner",
      region: "fsn1",
      status: SERVER_STATUS.running,
      meshIp: `10.8.0.1${i + 1}`,
    })),
    {
      id: FIXTURE.dbHostId,
      orgId: FIXTURE.orgId,
      name: "db-1",
      type: "database",
      provider: "hetzner",
      region: "fsn1",
      status: SERVER_STATUS.running,
      meshIp: "10.8.0.21",
    },
    {
      id: FIXTURE.storageHostId,
      orgId: FIXTURE.orgId,
      name: "store-1",
      type: "storage",
      provider: "hetzner",
      region: "fsn1",
      status: SERVER_STATUS.running,
      meshIp: "10.8.0.31",
    },
    {
      id: FIXTURE.unconnectedHostId,
      orgId: FIXTURE.orgId,
      name: "pending-1",
      type: "k8s",
      provider: "hetzner",
      region: "fsn1",
      status: SERVER_STATUS.provisioning,
    },
    {
      id: FIXTURE.incompatibleHostId,
      orgId: FIXTURE.orgId,
      name: "wrong-type-1",
      type: "gpu",
      provider: "hetzner",
      region: "fsn1",
      status: SERVER_STATUS.incompatible,
      incompatibleReasons: [
        {
          id: "gpu",
          fact: "gpus",
          expected: "an NVIDIA GPU with a working driver",
          detected: "none",
          reason: "wrong-type-1 reported no GPU, so it cannot enroll as a GPU server.",
        },
      ],
    },
    {
      id: FIXTURE.rivalHostId,
      orgId: FIXTURE.rivalOrgId,
      name: "rival-1",
      type: "k8s",
      provider: "hetzner",
      region: "fsn1",
      status: SERVER_STATUS.running,
      meshIp: "10.9.0.1",
    },
  ]);
}
