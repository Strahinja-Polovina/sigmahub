// What createResource actually writes down in demo mode.
//
// The wizard's choices reach this action and stop being choices: from here on
// they are columns, and anything it declines to persist is silently replaced by
// a default on the resource page. Two of those were open at once — the cluster
// target, fixed by SIGMA-215's migration, and the object-storage ENGINE, which
// was never written at all, so every demo user who picked SeaweedFS on the
// storage step opened their resource and was told MinIO.
//
// The refusal this file guards hardest is the other one: a cluster does not run
// a managed database, and the check that says so could be deleted here with
// every test still green. The wizard refuses those kinds on the target step,
// which means this is the refusal that has to hold when the request did not
// come from the wizard.

import { beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { eq } from "drizzle-orm";

import {
  CLUSTER_EXCLUDED_KINDS,
  DEFAULT_S3_ENGINE,
  S3_ENGINE_NAMES,
} from "@/lib/server-catalog.generated";
import { CLUSTER_STATUS, NODE_ROLE_CONTROL_PLANE } from "@/lib/demo-cluster";
import * as s from "@/server/db/schema";
import { FIXTURE, seedDemoFixture, type DemoDb } from "@/server/testing/demo-db";

vi.mock("@/server/db", async () => {
  const { createDemoDb } = await import("@/server/testing/demo-db");
  return { db: await createDemoDb() };
});
vi.mock("next/cache", () => ({ revalidatePath: () => {} }));
vi.mock("@/server/audit", () => ({ writeAudit: async () => {} }));
/** What the project gate does. `deny` makes requireProjectAdminForResource
 *  throw the same message the real one throws for a Developer (SIGMA-308);
 *  every other test leaves it null and the caller is an Org Admin. */
const gate = vi.hoisted(() => ({ deny: null as string | null }));
vi.mock("@/server/active-org", () => {
  const actor = { user: { id: "usr_you", name: "you" }, role: "Org Admin" };
  return {
    requireProjectRole: async () => actor,
    requireProjectAdminForResource: async () => {
      if (gate.deny) throw new Error(gate.deny);
      return actor;
    },
    requireResourceVisible: async () => {},
  };
});
// The read helpers, over the same database — @/server/queries itself is
// unimportable here (it pulls in the CP client, which is server-only).
vi.mock("@/server/queries", async () => {
  const { db } = await import("@/server/db");
  const schema = await import("@/server/db/schema");
  const { eq: is } = await import("drizzle-orm");
  return {
    getProject: async (id: string) =>
      (await db.select().from(schema.projects).where(is(schema.projects.id, id)))[0],
    getResource: async (id: string) =>
      (await db.select().from(schema.resources).where(is(schema.resources.id, id)))[0],
  };
});
vi.mock("@/server/cp", () => ({
  cpEnabled: () => false,
  cpCreateResource: async () => {
    throw new Error("the CP client must not be called in demo mode");
  },
  cpMirrorServer: async () => {
    throw new Error("the CP client must not be called in demo mode");
  },
  cpSelectGitRepo: async () => {},
  cpDeleteResource: async () => {},
  cpRedeploy: async () => ({ id: "" }),
  cpRequestConfirmToken: async () => ({ token: "", expiresAt: "" }),
  cpConfirmDestructive: async () => {},
  cpGetS3: async () => null,
  cpRevealS3Connection: async () => {
    throw new Error("the CP client must not be called in demo mode");
  },
  cpListBuckets: async () => [],
  cpCreateBucket: async () => ({}),
  cpDeleteBucket: async () => {},
  cpSetBucketQuota: async () => {},
  cpCreateBucketKey: async () => ({ accessKey: "" }),
}));
vi.mock("./domains", () => ({ attachDomain: async () => {} }));

import { createResource, deployResource } from "./resources";
import { getS3Info } from "./s3";

const CLUSTER_ID = "cls_shop_prod";
const [HOST_A] = FIXTURE.k8sHostIds;
/** An engine that is not the one an unset column falls back to — the whole
 *  point is that the user's pick survives, and a test using the default could
 *  not tell "persisted" from "defaulted". */
const PICKED_S3_ENGINE = S3_ENGINE_NAMES.find((e) => e !== DEFAULT_S3_ENGINE)!;
let db: DemoDb;

beforeAll(async () => {
  ({ db } = await import("@/server/db"));
  await seedDemoFixture(db);
}, 60_000);

beforeEach(async () => {
  gate.deny = null;
  await db.delete(s.deployments);
  await db.delete(s.resources);
  await db.delete(s.clusterNodes);
  await db.delete(s.clusters);
  await db.insert(s.clusters).values({
    id: CLUSTER_ID,
    orgId: FIXTURE.orgId,
    environmentId: FIXTURE.prodEnvId,
    name: "shop-prod",
    status: CLUSTER_STATUS.ready,
    createdBy: "you",
  });
  await db.insert(s.clusterNodes).values({
    clusterId: CLUSTER_ID,
    serverId: HOST_A,
    role: NODE_ROLE_CONTROL_PLANE,
    joinedAt: new Date(Date.now() - 86_400_000),
  });
});

const intoCluster = (overrides: Record<string, unknown> = {}) => ({
  projectId: FIXTURE.projectId,
  environmentId: FIXTURE.prodEnvId,
  clusterId: CLUSTER_ID,
  name: "api",
  kind: "app",
  ...overrides,
});

const ontoServer = (overrides: Record<string, unknown> = {}) => ({
  projectId: FIXTURE.projectId,
  environmentId: FIXTURE.prodEnvId,
  serverId: FIXTURE.storageHostId,
  name: "assets",
  kind: "s3",
  ...overrides,
});

describe("deploying into a demo cluster", () => {
  it("remembers which cluster the wizard aimed at", async () => {
    const { id } = await createResource(intoCluster());
    const [row] = await db.select().from(s.resources).where(eq(s.resources.id, id));
    expect(row.clusterId).toBe(CLUSTER_ID);
    expect(row.serverId).toBeNull();
  });

  // store.ClusterKindAllowed, from the catalog the control plane generates its
  // own copy of. Every one of these, so a kind added to the deny list on the Go
  // side is covered here the moment the catalog is regenerated.
  it.each([...CLUSTER_EXCLUDED_KINDS])(
    "refuses a %s, which runs on its own server and not inside a cluster",
    async (kind) => {
      await expect(createResource(intoCluster({ kind, name: `demo-${kind}` }))).rejects.toThrow(
        /runs on its own server, not inside a cluster/
      );
      expect(await db.select().from(s.resources)).toEqual([]);
    }
  );

  it("refuses a cluster that belongs to a different environment", async () => {
    await expect(
      createResource(intoCluster({ environmentId: FIXTURE.stagingEnvId }))
    ).rejects.toThrow(/belongs to a different environment/);
  });

  it("refuses a cluster id from another organization", async () => {
    await db.insert(s.clusters).values({
      id: "cls_rival",
      orgId: FIXTURE.rivalOrgId,
      environmentId: FIXTURE.prodEnvId,
      name: "rival",
      status: CLUSTER_STATUS.ready,
      createdBy: "them",
    });
    await expect(createResource(intoCluster({ clusterId: "cls_rival" }))).rejects.toThrow(
      /does not belong to this organization/
    );
  });

  it("refuses both a server and a cluster, because a resource runs in one place", async () => {
    await expect(
      createResource(intoCluster({ serverId: FIXTURE.storageHostId }))
    ).rejects.toThrow(/either a server or a cluster/);
  });

  it("refuses neither", async () => {
    await expect(createResource(intoCluster({ clusterId: null }))).rejects.toThrow(
      /deploy target is required/
    );
  });
});

describe("deploying onto a demo server", () => {
  it("refuses a host the enrollment gate marked incompatible with its type", async () => {
    await expect(
      createResource(ontoServer({ serverId: FIXTURE.incompatibleHostId, kind: "app" }))
    ).rejects.toThrow(/incompatible with its gpu type/);
  });

  it("refuses a server in another organization", async () => {
    await expect(createResource(ontoServer({ serverId: FIXTURE.rivalHostId }))).rejects.toThrow(
      /does not belong to this organization/
    );
  });

  it("refuses an environment that belongs to a different project", async () => {
    await db.insert(s.projects).values({
      id: "proj_other",
      orgId: FIXTURE.orgId,
      name: "Other",
      slug: "other",
    });
    await expect(createResource(ontoServer({ projectId: "proj_other" }))).rejects.toThrow(
      /Environment does not belong to this project/
    );
  });
});

describe("the engine a storage resource was created with", () => {
  it("is persisted, so the choice outlives the request that made it", async () => {
    const { id } = await createResource(ontoServer({ s3Engine: PICKED_S3_ENGINE }));
    const [row] = await db.select().from(s.resources).where(eq(s.resources.id, id));
    expect(row.engine).toBe(PICKED_S3_ENGINE);
  });

  // The bug as a user meets it: pick SeaweedFS in the wizard, open the
  // resource, and be told MinIO — the default, because nothing had written down
  // the answer and the panel had to guess one.
  it("is what the storage panel describes, not the catalog's default engine", async () => {
    const picked = await createResource(ontoServer({ s3Engine: PICKED_S3_ENGINE }));
    const info = await getS3Info({ orgId: FIXTURE.orgId, resourceId: picked.id });
    expect(info?.engine).toBe(PICKED_S3_ENGINE);

    const defaulted = await createResource(
      ontoServer({ name: "assets-2", s3Engine: DEFAULT_S3_ENGINE })
    );
    const other = await getS3Info({ orgId: FIXTURE.orgId, resourceId: defaulted.id });
    expect(other?.engine).toBe(DEFAULT_S3_ENGINE);
    // Not the same panel with a different label on it: the engine decides the
    // image, and it decides the endpoint an operator is told to dial.
    expect(info?.image).not.toBe(other?.image);
  });

  it("falls back to the default for a resource created before there was a column to hold it", async () => {
    const { id } = await createResource(ontoServer());
    const [row] = await db.select().from(s.resources).where(eq(s.resources.id, id));
    expect(row.engine).toBeNull();
    const info = await getS3Info({ orgId: FIXTURE.orgId, resourceId: id });
    expect(info?.engine).toBe(DEFAULT_S3_ENGINE);
  });

  it("is left unset for the kinds that have no engine to choose", async () => {
    const { id } = await createResource(intoCluster());
    const [row] = await db.select().from(s.resources).where(eq(s.resources.id, id));
    expect(row.engine).toBeNull();
  });

  it("carries the inference runtime for a model endpoint through the same field", async () => {
    const { id } = await createResource(
      ontoServer({
        serverId: FIXTURE.dbHostId,
        name: "chat",
        kind: "llm",
        llm: { engine: "vllm", model: "meta-llama/Llama-3.1-8B" },
      })
    );
    const [row] = await db.select().from(s.resources).where(eq(s.resources.id, id));
    expect(row.engine).toBe("vllm");
  });
});

describe("deployResource and the role it requires", () => {
  // SIGMA-308. The Project-Admin gate throws, and it throws BEFORE the
  // try/catch this action already has for exactly this reason: a refusal the
  // user must be able to read. Next.js redacts a thrown server-action message
  // in production, so a Developer's Deploy produced "An error occurred in the
  // Server Components render…" with a digest — during an incident, on a button
  // they were never allowed to press.
  it("returns a readable permission error for a Developer", async () => {
    const { id } = await createResource(intoCluster());
    const before = (await db.select().from(s.deployments)).length;
    gate.deny = "This action requires the Project Admin role for this project.";

    const res = await deployResource({ resourceId: id });

    expect(res).toHaveProperty("error");
    const message = (res as { error: string }).error;
    // It has to NAME the role, or it is one more sentence that doesn't say
    // what to do next.
    expect(message).toMatch(/Project Admin/);
    expect(message).not.toMatch(/digest|Server Components/i);
    // And a refused deploy is still a refused deploy: nothing was queued.
    expect(await db.select().from(s.deployments)).toHaveLength(before);
  });

  it("still queues a deployment for someone who holds the role", async () => {
    const { id } = await createResource(intoCluster());
    const before = (await db.select().from(s.deployments)).length;
    const res = await deployResource({ resourceId: id });
    expect(res).not.toHaveProperty("error");
    expect(await db.select().from(s.deployments)).toHaveLength(before + 1);
  });
});
