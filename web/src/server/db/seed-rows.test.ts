// The two decisions the demo seed makes before it writes anything.
//
// Both used to be statements inside the seed's main(), which is unimportable —
// it migrates, truncates and reseeds on import — so nothing could assert them.
// A reviewer emptied the fixture guard and hardcoded every seeded node to
// `ready`, and the whole suite stayed green while the demo grew a fleet that
// contradicts the panel rendering it.

import { describe, expect, it } from "vitest";

import { CLUSTER_EXCLUDED_KINDS } from "@/lib/server-catalog.generated";
import {
  CLUSTER_STATUS,
  DEMO_KUBERNETES_VERSION,
  DEMO_NODE_READY_MS,
  NODE_ROLE_CONTROL_PLANE,
  NODE_ROLE_WORKER,
  NODE_STATUS,
} from "@/lib/demo-cluster";
import { SERVER_STATUS } from "@/lib/server-compat";
import {
  assertResourceTargetsAreLegal,
  deriveSeededClusters,
  type SeedClusterFixture,
  type SeedHost,
} from "./seed-rows";

const NOW = Date.UTC(2027, 4, 1, 12, 0, 0);
const DAY = 86_400_000;

describe("the guard on the demo resource fixtures", () => {
  it("accepts a resource on a server and a resource in a cluster", () => {
    expect(() =>
      assertResourceTargetsAreLegal([
        { name: "api-postgres", kind: "postgres", serverId: "srv_db_1", clusterId: null },
        { name: "api-gateway", kind: "app", serverId: null, clusterId: "cls_api_prod" },
      ])
    ).not.toThrow();
  });

  // The control plane's own CHECK constraint (its migration 0050). A resource
  // with neither target renders as unassigned and one with both is a row the
  // API would have refused.
  it("refuses a resource with no deploy target at all, naming the file to fix", () => {
    expect(() =>
      assertResourceTargetsAreLegal([
        { name: "orphan", kind: "app", serverId: null, clusterId: null },
      ])
    ).toThrow(/orphan has 0 deploy targets[\s\S]*mock\/data\.ts/);
  });

  it("refuses a resource that names both a server and a cluster", () => {
    expect(() =>
      assertResourceTargetsAreLegal([
        { name: "greedy", kind: "app", serverId: "srv_gen_1", clusterId: "cls_api_prod" },
      ])
    ).toThrow(/greedy has 2 deploy targets/);
  });

  // Read from the generated catalog rather than listed here, so a kind the Go
  // side adds to the deny list lands in this guard — and in this test — for
  // free.
  it.each([...CLUSTER_EXCLUDED_KINDS])(
    "refuses a seeded %s inside a cluster, which the control plane would not schedule",
    (kind) => {
      expect(() =>
        assertResourceTargetsAreLegal([
          { name: `demo-${kind}`, kind, serverId: null, clusterId: "cls_api_prod" },
        ])
      ).toThrow(/will not schedule a .* inside one/);
    }
  );

  it("checks every fixture, not just the first", () => {
    expect(() =>
      assertResourceTargetsAreLegal([
        { name: "fine", kind: "app", serverId: "srv_gen_1" },
        { name: "broken", kind: "app" },
      ])
    ).toThrow(/broken/);
  });
});

const host = (id: string, status: string, meshIp: string | null = "10.8.0.1"): SeedHost => ({
  id,
  status,
  meshIp,
});

const fixture = (nodes: SeedClusterFixture["nodes"]): SeedClusterFixture => ({
  id: "cls_api_prod",
  orgId: "org_acme",
  environmentId: "env_api_prod",
  name: "api-prod",
  createdBy: "mila",
  createdDaysAgo: 30,
  nodes,
});

describe("the cluster rows the demo seed derives", () => {
  const controlPlane = { serverId: "srv_k8s_1", role: NODE_ROLE_CONTROL_PLANE, joinedDaysAgo: 30 };
  const worker = { serverId: "srv_k8s_2", role: NODE_ROLE_WORKER, joinedDaysAgo: 20 };

  it("reports a cluster whose hosts are all running as ready, at its mesh endpoint", () => {
    const { clusters, nodes } = deriveSeededClusters({
      clusters: [fixture([controlPlane, worker])],
      hosts: [
        host("srv_k8s_1", SERVER_STATUS.running, "10.8.0.11"),
        host("srv_k8s_2", SERVER_STATUS.running, "10.8.0.12"),
      ],
      seededAt: NOW,
    });
    expect(clusters[0].status).toBe(CLUSTER_STATUS.ready);
    expect(clusters[0].apiEndpoint).toBe("https://10.8.0.11:6443");
    expect(clusters[0].kubernetesVersion).toBe(DEMO_KUBERNETES_VERSION);
    expect(nodes.map((n) => n.nodeStatus)).toEqual([NODE_STATUS.ready, NODE_STATUS.ready]);
    expect(nodes.every((n) => n.reportedAt !== null)).toBe(true);
  });

  // The mutation this describe exists for. A seeded status is a claim about a
  // host, and the listing re-derives it from that host on the first render — so
  // a hardcoded `ready` is not an optimistic default, it is a row that the very
  // next page load contradicts.
  it("reports a node on an unreachable host as an error, and its cluster as degraded", () => {
    const { clusters, nodes } = deriveSeededClusters({
      clusters: [fixture([controlPlane, worker])],
      hosts: [
        host("srv_k8s_1", SERVER_STATUS.running, "10.8.0.11"),
        host("srv_k8s_2", SERVER_STATUS.unreachable),
      ],
      seededAt: NOW,
    });
    expect(clusters[0].status).toBe(CLUSTER_STATUS.degraded);
    const bad = nodes.find((n) => n.serverId === "srv_k8s_2")!;
    expect(bad.nodeStatus).toBe(NODE_STATUS.error);
    // The panel prints this under the node's name; an empty one is a badge with
    // no explanation.
    expect(bad.nodeMessage).not.toBe("");
  });

  it("reports a node on a host the enrollment gate refused as an error", () => {
    const { nodes } = deriveSeededClusters({
      clusters: [fixture([controlPlane, worker])],
      hosts: [
        host("srv_k8s_1", SERVER_STATUS.running, "10.8.0.11"),
        host("srv_k8s_2", SERVER_STATUS.incompatible),
      ],
      seededAt: NOW,
    });
    expect(nodes.find((n) => n.serverId === "srv_k8s_2")!.nodeStatus).toBe(NODE_STATUS.error);
  });

  it("leaves a cluster provisioning, with no endpoint and no version, until its control plane reports", () => {
    const { clusters, nodes } = deriveSeededClusters({
      clusters: [fixture([controlPlane, worker])],
      hosts: [
        host("srv_k8s_1", SERVER_STATUS.provisioning),
        host("srv_k8s_2", SERVER_STATUS.running, "10.8.0.12"),
      ],
      seededAt: NOW,
    });
    expect(clusters[0].status).toBe(CLUSTER_STATUS.provisioning);
    // An address nobody is listening on is worse than none: it is one someone
    // will try to curl.
    expect(clusters[0].apiEndpoint).toBe("");
    expect(clusters[0].kubernetesVersion).toBe("");
    const pending = nodes.find((n) => n.serverId === "srv_k8s_1")!;
    expect(pending.nodeStatus).toBe(NODE_STATUS.pending);
    // A node that has said nothing has no report to date.
    expect(pending.reportedAt).toBeNull();
  });

  it("leaves a node that joined seconds ago pending, because the install takes its time", () => {
    const { nodes } = deriveSeededClusters({
      clusters: [
        fixture([
          controlPlane,
          {
            serverId: "srv_k8s_2",
            role: NODE_ROLE_WORKER,
            joinedDaysAgo: (DEMO_NODE_READY_MS / 2) / DAY,
          },
        ]),
      ],
      hosts: [
        host("srv_k8s_1", SERVER_STATUS.running, "10.8.0.11"),
        host("srv_k8s_2", SERVER_STATUS.running, "10.8.0.12"),
      ],
      seededAt: NOW,
    });
    expect(nodes.find((n) => n.serverId === "srv_k8s_2")!.nodeStatus).toBe(NODE_STATUS.pending);
  });

  it("does not call a node on a host that is not in the fleet ready", () => {
    const { clusters, nodes } = deriveSeededClusters({
      clusters: [fixture([controlPlane, { ...worker, serverId: "srv_ghost" }])],
      hosts: [host("srv_k8s_1", SERVER_STATUS.running, "10.8.0.11")],
      seededAt: NOW,
    });
    expect(nodes.find((n) => n.serverId === "srv_ghost")!.nodeStatus).toBe(NODE_STATUS.pending);
    expect(clusters[0].status).toBe(CLUSTER_STATUS.degraded);
  });

  it("measures every clock from the one instant the database was seeded", () => {
    const { clusters, nodes } = deriveSeededClusters({
      clusters: [fixture([controlPlane, worker])],
      hosts: [
        host("srv_k8s_1", SERVER_STATUS.running, "10.8.0.11"),
        host("srv_k8s_2", SERVER_STATUS.running, "10.8.0.12"),
      ],
      seededAt: NOW,
    });
    expect(clusters[0].createdAt.getTime()).toBe(NOW - 30 * DAY);
    expect(nodes.find((n) => n.serverId === "srv_k8s_2")!.joinedAt.getTime()).toBe(NOW - 20 * DAY);
  });
});
