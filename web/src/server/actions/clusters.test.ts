// Demo mode's cluster rules, and the path that carries them to the wizard.
//
// Every refusal below is a second implementation of one the control plane
// makes, and until this file existed none of them was covered by anything: a
// reviewer deleted the one-cluster-per-environment check, the already-claimed
// check and the control-plane-node refusal, and all 468 tests stayed green. The
// exclusion list was worse than uncovered — returning `[]` for it does not look
// like a deletion, it looks like "no kinds are excluded", and the control plane
// reads that as "a cluster hosts anything". The wizard would then offer a
// cluster as a target for a Postgres that createResource itself rejects.
//
// The actions run here against a real migrated PGlite database (see
// @/server/testing/demo-db) rather than against extracted helpers, because the
// thing that broke was never a helper. It was the wiring.

import { readFileSync } from "node:fs";
import { join } from "node:path";
import { beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { eq } from "drizzle-orm";

import { CLUSTER_EXCLUDED_KINDS } from "@/lib/server-catalog.generated";
import {
  CLUSTER_STATUS,
  DEMO_NODE_READY_MS,
  NODE_ROLE_CONTROL_PLANE,
  NODE_ROLE_WORKER,
  NODE_STATUS,
} from "@/lib/demo-cluster";
import * as s from "@/server/db/schema";
import { FIXTURE, seedDemoFixture, type DemoDb } from "@/server/testing/demo-db";

// The database is the only dependency the demo branch actually uses; identity,
// the audit log, the router cache and the CP client are mocked away because the
// demo branch either does not reach them or does not decide anything with them.
vi.mock("@/server/db", async () => {
  const { createDemoDb } = await import("@/server/testing/demo-db");
  return { db: await createDemoDb() };
});
vi.mock("next/cache", () => ({ revalidatePath: () => {} }));
vi.mock("@/server/audit", () => ({ writeAudit: async () => {} }));
vi.mock("@/server/active-org", () => {
  const actor = { user: { id: "usr_you", name: "you" }, role: "Org Admin" };
  return {
    requireMembership: async () => actor,
    requireProjectAdmin: async () => actor,
    requireEnvironmentVisible: async () => {},
  };
});
// cpEnabled() false is the branch under test: with a control plane every one of
// these decisions belongs to the CP and this module only forwards.
vi.mock("@/server/cp", () => ({
  cpEnabled: () => false,
  cpListClusters: async () => {
    throw new Error("the CP client must not be called in demo mode");
  },
  cpCreateCluster: async () => {
    throw new Error("the CP client must not be called in demo mode");
  },
  cpAddClusterNode: async () => {
    throw new Error("the CP client must not be called in demo mode");
  },
  cpRemoveClusterNode: async () => {
    throw new Error("the CP client must not be called in demo mode");
  },
  cpDeleteCluster: async () => {
    throw new Error("the CP client must not be called in demo mode");
  },
}));

import {
  addClusterNode,
  createCluster,
  deleteCluster,
  listClusters,
  removeClusterNode,
} from "./clusters";

const [HOST_A, HOST_B, HOST_C] = FIXTURE.k8sHostIds;
let db: DemoDb;

/** A cluster with a ready control plane, written straight to the tables so the
 *  test starts from a state createCluster is not the only way to reach. */
async function existingCluster(input: {
  id: string;
  environmentId: string;
  name: string;
  controlPlaneId: string;
  workerIds?: string[];
}) {
  const longAgo = new Date(Date.now() - 10 * DEMO_NODE_READY_MS);
  await db.insert(s.clusters).values({
    id: input.id,
    orgId: FIXTURE.orgId,
    environmentId: input.environmentId,
    name: input.name,
    status: CLUSTER_STATUS.provisioning,
    createdBy: "you",
    createdAt: longAgo,
  });
  await db.insert(s.clusterNodes).values([
    {
      clusterId: input.id,
      serverId: input.controlPlaneId,
      role: NODE_ROLE_CONTROL_PLANE,
      joinedAt: longAgo,
    },
    ...(input.workerIds ?? []).map((serverId) => ({
      clusterId: input.id,
      serverId,
      role: NODE_ROLE_WORKER,
      joinedAt: longAgo,
    })),
  ]);
}

beforeAll(async () => {
  ({ db } = await import("@/server/db"));
  await seedDemoFixture(db);
}, 60_000);

beforeEach(async () => {
  await db.delete(s.resources);
  await db.delete(s.clusterNodes);
  await db.delete(s.clusters);
});

describe("what the cluster listing tells the New Resource wizard", () => {
  it("names the kinds a cluster refuses, even when there is not a single cluster to list", async () => {
    const { clusters, excludedKinds } = await listClusters(FIXTURE.orgId);
    expect(clusters).toEqual([]);
    expect(excludedKinds).toEqual([...CLUSTER_EXCLUDED_KINDS]);
  });

  // The mutation that motivated this file. An empty array is not a smaller
  // answer than the real list, it is the OPPOSITE one: the control plane reads
  // excludedKinds as a DENY list, so `[]` means every kind is allowed and the
  // wizard offers a cluster for a Postgres the create call then refuses.
  it("never answers with an empty exclusion list, which reads as 'a cluster hosts anything'", async () => {
    const { excludedKinds } = await listClusters(FIXTURE.orgId);
    expect(excludedKinds.length).toBeGreaterThan(0);
    expect(excludedKinds).toContain("postgres");
  });

  it("still names them when the listing is filtered to an environment that has no cluster", async () => {
    await existingCluster({
      id: "cls_prod",
      environmentId: FIXTURE.prodEnvId,
      name: "prod",
      controlPlaneId: HOST_A,
    });
    const { clusters, excludedKinds } = await listClusters(FIXTURE.orgId, FIXTURE.stagingEnvId);
    expect(clusters).toEqual([]);
    expect(excludedKinds).toEqual([...CLUSTER_EXCLUDED_KINDS]);
  });

  it("derives each node's report from its host rather than reading the stored row back", async () => {
    await existingCluster({
      id: "cls_prod",
      environmentId: FIXTURE.prodEnvId,
      name: "prod",
      controlPlaneId: HOST_A,
      workerIds: [HOST_B],
    });
    const { clusters } = await listClusters(FIXTURE.orgId);
    expect(clusters).toHaveLength(1);
    expect(clusters[0].status).toBe(CLUSTER_STATUS.ready);
    // Control plane first: it is the node the whole card is about.
    expect(clusters[0].nodes.map((n) => n.role)).toEqual([
      NODE_ROLE_CONTROL_PLANE,
      NODE_ROLE_WORKER,
    ]);
    expect(clusters[0].nodes.every((n) => n.nodeStatus === NODE_STATUS.ready)).toBe(true);
  });

  it("does not list another organization's clusters", async () => {
    await existingCluster({
      id: "cls_prod",
      environmentId: FIXTURE.prodEnvId,
      name: "prod",
      controlPlaneId: HOST_A,
    });
    const { clusters } = await listClusters(FIXTURE.rivalOrgId);
    expect(clusters).toEqual([]);
  });
});

describe("building a demo cluster", () => {
  it("promotes the chosen server into the control plane", async () => {
    const cluster = await createCluster({
      orgId: FIXTURE.orgId,
      environmentId: FIXTURE.prodEnvId,
      name: "prod",
      controlPlaneId: HOST_A,
    });
    expect(cluster.nodes).toHaveLength(1);
    expect(cluster.nodes[0]).toMatchObject({
      serverId: HOST_A,
      role: NODE_ROLE_CONTROL_PLANE,
    });
    // Nothing is ready the instant it is created — the install takes its time,
    // and a cluster that reported ready immediately would delete the state the
    // panel exists to show.
    expect(cluster.status).toBe(CLUSTER_STATUS.provisioning);
    expect(cluster.apiEndpoint).toBe("");
  });

  // One cluster per environment, so "deploy to the cluster" is unambiguous. A
  // second row would be shown twice on the wizard's target step and the deploy
  // would go to whichever the environment filter happened to return first.
  it("refuses a second cluster in an environment that already has one, and names it", async () => {
    await existingCluster({
      id: "cls_prod",
      environmentId: FIXTURE.prodEnvId,
      name: "shop-prod",
      controlPlaneId: HOST_A,
    });
    await expect(
      createCluster({
        orgId: FIXTURE.orgId,
        environmentId: FIXTURE.prodEnvId,
        name: "second",
        controlPlaneId: HOST_B,
      })
    ).rejects.toThrow(/already has a cluster \(shop-prod\)/);
    const rows = await db.select().from(s.clusters);
    expect(rows).toHaveLength(1);
  });

  it("allows a cluster in a second environment of the same project", async () => {
    await existingCluster({
      id: "cls_prod",
      environmentId: FIXTURE.prodEnvId,
      name: "shop-prod",
      controlPlaneId: HOST_A,
    });
    const cluster = await createCluster({
      orgId: FIXTURE.orgId,
      environmentId: FIXTURE.stagingEnvId,
      name: "shop-staging",
      controlPlaneId: HOST_B,
    });
    expect(cluster.environmentId).toBe(FIXTURE.stagingEnvId);
  });

  // A server runs one Kubernetes. Promoting a host that is already a node
  // would leave it a member of two clusters, each of which believes it owns it.
  it("refuses a server that already belongs to a cluster, and names it", async () => {
    await existingCluster({
      id: "cls_prod",
      environmentId: FIXTURE.prodEnvId,
      name: "shop-prod",
      controlPlaneId: HOST_A,
    });
    await expect(
      createCluster({
        orgId: FIXTURE.orgId,
        environmentId: FIXTURE.stagingEnvId,
        name: "shop-staging",
        controlPlaneId: HOST_A,
      })
    ).rejects.toThrow(/k8s-1 already belongs to a cluster/);
  });

  it("refuses a host the agent has never checked in from, because the agent installs Kubernetes", async () => {
    await expect(
      createCluster({
        orgId: FIXTURE.orgId,
        environmentId: FIXTURE.prodEnvId,
        name: "prod",
        controlPlaneId: FIXTURE.unconnectedHostId,
      })
    ).rejects.toThrow(/has not checked in/);
  });

  it("refuses a server in another organization", async () => {
    await expect(
      createCluster({
        orgId: FIXTURE.orgId,
        environmentId: FIXTURE.prodEnvId,
        name: "prod",
        controlPlaneId: FIXTURE.rivalHostId,
      })
    ).rejects.toThrow(/not in this organization/);
  });

  it("requires a name, rather than creating a cluster nothing can refer to", async () => {
    await expect(
      createCluster({
        orgId: FIXTURE.orgId,
        environmentId: FIXTURE.prodEnvId,
        name: "   ",
        controlPlaneId: HOST_A,
      })
    ).rejects.toThrow(/name is required/);
  });
});

describe("joining a server to a demo cluster", () => {
  beforeEach(async () => {
    await existingCluster({
      id: "cls_prod",
      environmentId: FIXTURE.prodEnvId,
      name: "shop-prod",
      controlPlaneId: HOST_A,
    });
  });

  it("adds it as a worker", async () => {
    await addClusterNode({ orgId: FIXTURE.orgId, clusterId: "cls_prod", serverId: HOST_B });
    const nodes = await db
      .select()
      .from(s.clusterNodes)
      .where(eq(s.clusterNodes.serverId, HOST_B));
    expect(nodes).toHaveLength(1);
    expect(nodes[0].role).toBe(NODE_ROLE_WORKER);
  });

  it("refuses a server that already belongs to another cluster", async () => {
    await existingCluster({
      id: "cls_staging",
      environmentId: FIXTURE.stagingEnvId,
      name: "shop-staging",
      controlPlaneId: HOST_C,
    });
    await expect(
      addClusterNode({ orgId: FIXTURE.orgId, clusterId: "cls_prod", serverId: HOST_C })
    ).rejects.toThrow(/already belongs to a cluster/);
  });

  it("refuses a cluster id from another organization, which has no tenancy boundary of its own", async () => {
    await expect(
      addClusterNode({ orgId: FIXTURE.rivalOrgId, clusterId: "cls_prod", serverId: HOST_B })
    ).rejects.toThrow(/Cluster not found/);
  });
});

describe("removing a node from a demo cluster", () => {
  beforeEach(async () => {
    await existingCluster({
      id: "cls_prod",
      environmentId: FIXTURE.prodEnvId,
      name: "shop-prod",
      controlPlaneId: HOST_A,
      workerIds: [HOST_B],
    });
  });

  it("drains a worker", async () => {
    await removeClusterNode({ orgId: FIXTURE.orgId, clusterId: "cls_prod", serverId: HOST_B });
    const nodes = await db.select().from(s.clusterNodes);
    expect(nodes.map((n) => n.serverId)).toEqual([HOST_A]);
  });

  // store.ErrControlPlaneNode: draining the node that runs the API server does
  // not shrink the cluster, it ends it. Without this refusal the demo happily
  // deletes the control plane and leaves a cluster whose every read then says
  // `provisioning` forever, with no way back.
  it("refuses the control-plane node and says to delete the cluster instead", async () => {
    await expect(
      removeClusterNode({ orgId: FIXTURE.orgId, clusterId: "cls_prod", serverId: HOST_A })
    ).rejects.toThrow(/control-plane node cannot be removed; delete the cluster instead/);
    const nodes = await db.select().from(s.clusterNodes);
    expect(nodes.map((n) => n.serverId).sort()).toEqual([HOST_A, HOST_B].sort());
  });

  it("refuses a server that is not a node of this cluster", async () => {
    await expect(
      removeClusterNode({ orgId: FIXTURE.orgId, clusterId: "cls_prod", serverId: HOST_C })
    ).rejects.toThrow(/not a node of this cluster/);
  });
});

describe("deleting a demo cluster", () => {
  it("leaves the workloads behind, stopped, rather than deleting them with it", async () => {
    await existingCluster({
      id: "cls_prod",
      environmentId: FIXTURE.prodEnvId,
      name: "shop-prod",
      controlPlaneId: HOST_A,
    });
    await db.insert(s.resources).values({
      id: "res_api",
      projectId: FIXTURE.projectId,
      environmentId: FIXTURE.prodEnvId,
      clusterId: "cls_prod",
      name: "api",
      kind: "app",
      status: "running",
    });

    await deleteCluster({ orgId: FIXTURE.orgId, clusterId: "cls_prod" });

    const [resource] = await db.select().from(s.resources).where(eq(s.resources.id, "res_api"));
    expect(resource).toBeDefined();
    expect(resource.clusterId).toBeNull();
    expect(resource.status).toBe("stopped");
    expect(await db.select().from(s.clusters)).toEqual([]);
  });
});

// The rule above is only worth having if it reaches the wizard. It is carried
// there as a prop, through three pages and three views, and every component in
// the chain defaults it to `[]` so that a call site which simply forgets it
// says "a cluster hosts anything" — silently, and in the one direction that
// re-creates the original bug. Nothing type-checks that: the prop is optional
// and `[]` is a valid string[].
describe("the pages that offer a cluster as a deploy target", () => {
  const SRC = join(process.cwd(), "src");
  const CALL_SITES = [
    join("app", "(app)", "dashboard", "resources", "page.tsx"),
    join("app", "(app)", "dashboard", "projects", "[projectId]", "page.tsx"),
    join("app", "(app)", "dashboard", "servers", "page.tsx"),
    join("components", "dashboard", "resources", "resources-view.tsx"),
    join("components", "dashboard", "projects", "project-detail-view.tsx"),
    join("components", "dashboard", "servers", "servers-view.tsx"),
  ];
  const source = (rel: string) => readFileSync(join(SRC, rel), "utf8");

  it("ask the listing for the clusters AND for the kinds they refuse", () => {
    const pages = CALL_SITES.filter((rel) => rel.startsWith("app"));
    expect(pages.length).toBeGreaterThan(0);
    for (const rel of pages) {
      const text = source(rel);
      expect(text, `${rel} must call listClusters`).toMatch(/listClusters\(/);
      expect(text, `${rel} must pass on the excludedKinds it answered with`).toMatch(
        /xcludedKinds=\{[^}]*excludedKinds/
      );
    }
  });

  it("hand both onwards together, so no view is left guessing what a cluster refuses", () => {
    for (const rel of CALL_SITES) {
      const text = source(rel);
      if (!/\bclusters=\{/.test(text)) continue;
      expect(text, `${rel} passes clusters without the kinds they refuse`).toMatch(
        /xcludedKinds=\{/
      );
    }
  });

  it("never hard-code the empty exclusion list, which means the opposite of empty", () => {
    for (const rel of CALL_SITES) {
      expect(source(rel), `${rel} hard-codes an empty exclusion list`).not.toMatch(
        /xcludedKinds=\{\s*\[\s*\]\s*\}/
      );
    }
  });
});
