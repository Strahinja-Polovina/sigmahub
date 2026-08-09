/**
 * The rows the demo seed writes, decided without a database (SIGMA-215).
 *
 * The seed script is a `main()` that migrates, truncates and inserts, so
 * nothing it decided along the way could be asserted: the guard that refuses an
 * illegal fixture and the derivation that turns a cluster fixture into cluster
 * and node rows were both statements in the middle of two hundred inserts.
 * Deleting either left every check green and produced a demo that teaches a
 * rule the product does not have — a resource in a cluster the control plane
 * would refuse to schedule it into, or a fleet of nodes that report `ready`
 * whatever their hosts are doing.
 *
 * So they live here, as functions over plain values. The fixture shapes are
 * restated structurally rather than imported from @/lib/mock/types for the same
 * reason @/lib/deploy-spec restates the detected-compose shape: this module
 * decides things ABOUT fixtures and should not own a dependency on the file
 * that happens to hold today's.
 */

import { clusterCanHost } from "@/lib/server-catalog.generated";
import {
  CLUSTER_STATUS,
  NODE_ROLE_CONTROL_PLANE,
  demoApiEndpoint,
  demoClusterStatus,
  demoKubernetesVersion,
  demoNodeReport,
} from "@/lib/demo-cluster";
import { SERVER_STATUS } from "@/lib/server-compat";

const DAY = 86_400_000;

/** The part of a resource fixture that says where it runs. */
export type SeedResourceTarget = {
  name: string;
  kind: string;
  serverId?: string | null;
  clusterId?: string | null;
};

/**
 * Refuse to seed a fixture the product itself would not have accepted.
 *
 * Demo data is the first thing a prospective user sees, so a row the wizard
 * could never have produced is worse than a missing one: it teaches a rule and
 * then breaks it on the next screen. Both checks are the control plane's own —
 * one target per resource (its migration 0050's CHECK constraint), and the
 * kinds a cluster refuses, read from the generated catalog rather than listed
 * here so a change on the Go side lands in this guard for free.
 *
 * Throws rather than returning, and does so before the first insert, so a bad
 * fixture fails at the top of the seed with a sentence naming the file to fix
 * instead of as a foreign-key error two hundred rows later.
 */
export function assertResourceTargetsAreLegal(resources: readonly SeedResourceTarget[]): void {
  for (const r of resources) {
    const targets = [r.serverId, r.clusterId].filter(Boolean).length;
    if (targets !== 1) {
      throw new Error(
        `Demo resource ${r.name} has ${targets} deploy targets. Set exactly one of serverId or clusterId on it in web/src/lib/mock/data.ts.`
      );
    }
    if (r.clusterId && !clusterCanHost(r.kind)) {
      throw new Error(
        `Demo resource ${r.name} is seeded into a cluster, but the control plane will not schedule a ${r.kind} inside one. Give it a serverId instead in web/src/lib/mock/data.ts.`
      );
    }
  }
}

/** A cluster fixture: what was decided by hand, in offsets rather than dates. */
export type SeedClusterFixture = {
  id: string;
  orgId: string;
  environmentId: string;
  name: string;
  createdBy: string;
  createdDaysAgo: number;
  nodes: { serverId: string; role: string; joinedDaysAgo: number }[];
};

/** A seeded host, as the seed left it — its status is DERIVED there (the
 *  enrollment gate and the teardown clock both outrank the fixture), which is
 *  exactly why the node rows below are derived from these and not from the
 *  fixture's own idea of its hosts. */
export type SeedHost = { id: string; status: string; meshIp: string | null };

export type SeededClusterRow = {
  id: string;
  orgId: string;
  environmentId: string;
  name: string;
  status: string;
  apiEndpoint: string;
  kubernetesVersion: string;
  createdBy: string;
  createdAt: Date;
};

export type SeededClusterNodeRow = {
  clusterId: string;
  serverId: string;
  role: string;
  nodeStatus: string;
  nodeMessage: string;
  joinedAt: Date;
  reportedAt: Date | null;
};

/**
 * A cluster fixture's rows, derived from its nodes' HOSTS by the demo's own
 * functions rather than by a second copy of the rule here.
 *
 * The listing re-derives all of this on every read — a node's report comes from
 * the host's status and how long ago it joined (demoNodeReport), and the
 * cluster's status from the reports (demoClusterStatus, the TypeScript half of
 * store.rederiveClusterStatusTx). So these columns are not what the dashboard
 * will show; they are what it will show, written down. A seeded `ready` node
 * over an unreachable host would be corrected on the first render, and a stored
 * status that disagreed with the panel would send whoever next opened this
 * database looking for a bug that is not there.
 *
 * Every clock is measured from the single `seededAt` instant, so one seed run
 * cannot produce a fleet whose in-flight states were each captured a few
 * milliseconds apart.
 */
export function deriveSeededClusters(input: {
  clusters: readonly SeedClusterFixture[];
  hosts: readonly SeedHost[];
  seededAt: number;
}): { clusters: SeededClusterRow[]; nodes: SeededClusterNodeRow[] } {
  const host = new Map(input.hosts.map((h) => [h.id, h]));
  const clusters: SeededClusterRow[] = [];
  const nodes: SeededClusterNodeRow[] = [];

  for (const fixture of input.clusters) {
    const derived = fixture.nodes.map((node) => {
      const joinedAt = new Date(input.seededAt - node.joinedDaysAgo * DAY);
      const report = demoNodeReport({
        joinedAt,
        // A node whose host is not in the fleet at all is not `ready` by
        // default: it has nothing running on it to report.
        serverStatus: host.get(node.serverId)?.status ?? SERVER_STATUS.provisioning,
        now: input.seededAt,
      });
      return { node, joinedAt, report };
    });

    const status = demoClusterStatus(
      derived.map(({ node, report }) => ({ role: node.role, status: report.status }))
    );
    const controlPlane = derived.find(({ node }) => node.role === NODE_ROLE_CONTROL_PLANE);
    clusters.push({
      id: fixture.id,
      orgId: fixture.orgId,
      environmentId: fixture.environmentId,
      name: fixture.name,
      status,
      // Empty while provisioning: the API server is not answering yet, and a
      // placeholder URL there is an address someone can try to curl.
      apiEndpoint:
        status === CLUSTER_STATUS.provisioning
          ? ""
          : demoApiEndpoint(controlPlane ? host.get(controlPlane.node.serverId)?.meshIp : ""),
      kubernetesVersion: demoKubernetesVersion(status),
      createdBy: fixture.createdBy,
      createdAt: new Date(input.seededAt - fixture.createdDaysAgo * DAY),
    });

    for (const { node, joinedAt, report } of derived) {
      nodes.push({
        clusterId: fixture.id,
        serverId: node.serverId,
        role: node.role,
        nodeStatus: report.status,
        nodeMessage: report.message,
        joinedAt,
        // A node still `pending` has said nothing about Kubernetes yet, and a
        // timestamp there would date a report that does not exist.
        reportedAt: report.status === "pending" ? null : new Date(input.seededAt),
      });
    }
  }
  return { clusters, nodes };
}
