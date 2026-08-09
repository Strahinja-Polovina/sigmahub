"use server";

// Kubernetes clusters, in both modes (SIGMA-215).
//
// This module used to open with an ensureCp() that every function called, so
// with no control plane the whole vertical threw — and the three pages that
// list clusters worked around it by not calling in at all, hard-coding an empty
// cluster list and an empty exclusion list at the call site. Both halves of that
// workaround were wrong: no cluster could be seen, built or deployed into, and
// an empty exclusion list is the control plane's answer for "every kind is
// allowed", which would have offered a cluster as a target for a database the
// API refuses.
//
// The demo branch is a real implementation over the local clusters/cluster_nodes
// tables, and the node/cluster state it reports is derived from the rows' own
// timestamps — see @/lib/demo-cluster for why, and for the timescale.

import { revalidatePath } from "next/cache";
import { and, eq, inArray } from "drizzle-orm";
import { db } from "../db";
import * as s from "../db/schema";
import {
  requireMembership,
  requireProjectAdmin,
  requireEnvironmentVisible,
} from "../active-org";
import { writeAudit } from "../audit";
import { CLUSTER_EXCLUDED_KINDS } from "@/lib/server-catalog.generated";
import {
  CLUSTER_STATUS,
  NODE_ROLE_CONTROL_PLANE,
  NODE_ROLE_WORKER,
  controlPlaneRefusal,
  demoApiEndpoint,
  demoClusterStatus,
  demoKubernetesVersion,
  demoNodeReport,
} from "@/lib/demo-cluster";
import {
  cpEnabled,
  cpListClusters,
  cpCreateCluster,
  cpAddClusterNode,
  cpRemoveClusterNode,
  cpDeleteCluster,
  type CpCluster,
  type CpClusterNode,
} from "../cp";

function rid(prefix: string) {
  return `${prefix}_${crypto.randomUUID().replace(/-/g, "").slice(0, 12)}`;
}

/** The kinds a cluster refuses, as the generated catalog publishes them.
 *
 *  In CP mode the authoritative answer travels with the listing, because the
 *  control plane may be running a build whose deny list has moved on. This is
 *  what stands in for it when there is no listing to read — with no control
 *  plane, or when the one we have could not be reached. Never an empty array:
 *  the control plane reads the list as a DENY list, so empty means "a cluster
 *  hosts anything", and a wizard told that offers a Postgres a target its own
 *  create call rejects. */
function catalogExcludedKinds(): string[] {
  return [...CLUSTER_EXCLUDED_KINDS];
}

/** One demo cluster, assembled into the shape the control plane answers with.
 *
 *  The node list is joined against the servers table rather than stored
 *  denormalised: a node's name, type and mesh address are the SERVER's, and a
 *  copy of them here would go stale the first time a demo host was renamed by
 *  its own check-in — which is a thing that happens on the connect path. */
async function demoCluster(
  row: typeof s.clusters.$inferSelect,
  now: number
): Promise<CpCluster> {
  const nodeRows = await db
    .select({
      node: s.clusterNodes,
      server: s.servers,
    })
    .from(s.clusterNodes)
    .innerJoin(s.servers, eq(s.clusterNodes.serverId, s.servers.id))
    .where(eq(s.clusterNodes.clusterId, row.id));

  const nodes: CpClusterNode[] = nodeRows
    .map(({ node, server }) => {
      const report = demoNodeReport({
        joinedAt: node.joinedAt,
        serverStatus: server.status,
        now,
      });
      return {
        serverId: server.id,
        serverName: server.name,
        serverType: server.type,
        status: server.status,
        meshIp: server.meshIp,
        role: node.role,
        joinedAt: node.joinedAt.toISOString(),
        nodeStatus: report.status,
        nodeMessage: report.message,
        // When the node last said something about Kubernetes. A pending node
        // has said nothing yet, and a timestamp there would date a report that
        // does not exist.
        reportedAt: report.status === "pending" ? null : new Date(now).toISOString(),
      };
    })
    // Control plane first: it is the node the whole card is about, and a list
    // ordered by insertion put it wherever the seed happened to write it.
    .sort((a, b) => {
      if (a.role === b.role) return a.serverName.localeCompare(b.serverName);
      return a.role === NODE_ROLE_CONTROL_PLANE ? -1 : 1;
    });

  const status = demoClusterStatus(nodes.map((n) => ({ role: n.role, status: n.nodeStatus })));
  const controlPlane = nodes.find((n) => n.role === NODE_ROLE_CONTROL_PLANE);
  return {
    id: row.id,
    orgId: row.orgId,
    environmentId: row.environmentId,
    name: row.name,
    // Derived, never read back from the column. The row's stored status is the
    // value the insert happened to write; the nodes are what is true now, and
    // the control plane re-derives it on every report for the same reason.
    status,
    apiEndpoint: status === CLUSTER_STATUS.provisioning ? "" : demoApiEndpoint(controlPlane?.meshIp),
    kubernetesVersion: demoKubernetesVersion(status),
    createdBy: row.createdBy,
    createdAt: row.createdAt.toISOString(),
    nodes,
  };
}

/** The org's clusters, plus which resource kinds a cluster refuses.
 *
 *  Member-visible in both modes: a cluster is a deploy target, and a developer
 *  who cannot see the targets cannot fill in the deploy form. */
export async function listClusters(
  orgId: string,
  environmentId?: string
): Promise<{ clusters: CpCluster[]; excludedKinds: string[] }> {
  await requireMembership(orgId);
  if (cpEnabled()) {
    try {
      return await cpListClusters(orgId, environmentId);
    } catch {
      // The clusters genuinely cannot be listed, but the RULE about them is
      // rendered from the same catalog the control plane generates its own
      // from, so it is still known. Returning it keeps the wizard's refusal
      // honest on a screen that has lost its cluster rows.
      return { clusters: [], excludedKinds: catalogExcludedKinds() };
    }
  }

  const rows = environmentId
    ? await db
        .select()
        .from(s.clusters)
        .where(and(eq(s.clusters.orgId, orgId), eq(s.clusters.environmentId, environmentId)))
    : await db.select().from(s.clusters).where(eq(s.clusters.orgId, orgId));
  const now = Date.now();
  const clusters = await Promise.all(rows.map((row) => demoCluster(row, now)));
  clusters.sort((a, b) => a.name.localeCompare(b.name));
  return { clusters, excludedKinds: catalogExcludedKinds() };
}

/** A server the demo mode may promote or join, or the sentence refusing it. */
async function demoJoinableServer(orgId: string, serverId: string) {
  const [server] = await db
    .select()
    .from(s.servers)
    .where(and(eq(s.servers.id, serverId), eq(s.servers.orgId, orgId)));
  if (!server) throw new Error("That server is not in this organization.");
  const [claimed] = await db
    .select({ clusterId: s.clusterNodes.clusterId })
    .from(s.clusterNodes)
    .where(eq(s.clusterNodes.serverId, serverId));
  if (claimed) {
    throw new Error(`${server.name} already belongs to a cluster.`);
  }
  const refusal = controlPlaneRefusal(server);
  if (refusal) throw new Error(refusal);
  return server;
}

/** Build a cluster, promoting the chosen server into its control plane. */
export async function createCluster(input: {
  orgId: string;
  environmentId: string;
  name: string;
  controlPlaneId: string;
}): Promise<CpCluster> {
  await requireEnvironmentVisible(input.orgId, input.environmentId);
  const { user, role } = await requireProjectAdmin(input.orgId);
  const name = input.name.trim();
  if (!name) throw new Error("A cluster name is required.");

  if (cpEnabled()) {
    const cluster = await cpCreateCluster(
      input.orgId,
      {
        environmentId: input.environmentId,
        name,
        controlPlaneId: input.controlPlaneId,
      },
      { name: user.name, role }
    );
    await writeAudit({
      orgId: input.orgId,
      actor: user.name,
      action: "Created cluster",
      target: cluster.name,
    });
    revalidatePath("/dashboard", "layout");
    return cluster;
  }

  // One cluster per environment, so "deploy to the cluster" is unambiguous —
  // the same conflict the control plane answers with, made here rather than
  // letting a second row exist that the environment filter would then show
  // twice on the target step.
  const [existing] = await db
    .select({ name: s.clusters.name })
    .from(s.clusters)
    .where(eq(s.clusters.environmentId, input.environmentId));
  if (existing) {
    throw new Error(`This environment already has a cluster (${existing.name}).`);
  }
  const server = await demoJoinableServer(input.orgId, input.controlPlaneId);

  const id = rid("cls");
  const now = new Date();
  await db.insert(s.clusters).values({
    id,
    orgId: input.orgId,
    environmentId: input.environmentId,
    name,
    // The stored value is the starting point; every read re-derives it from
    // the nodes, exactly as rederiveClusterStatusTx does in the control plane.
    status: CLUSTER_STATUS.provisioning,
    createdBy: user.name,
    createdAt: now,
  });
  await db.insert(s.clusterNodes).values({
    clusterId: id,
    serverId: server.id,
    role: NODE_ROLE_CONTROL_PLANE,
    joinedAt: now,
  });
  await writeAudit({
    orgId: input.orgId,
    actor: user.name,
    action: "Created cluster",
    target: name,
  });
  revalidatePath("/dashboard", "layout");
  const [row] = await db.select().from(s.clusters).where(eq(s.clusters.id, id));
  return demoCluster(row, now.getTime());
}

/** Resolve a cluster inside this org, so a client-supplied cluster id from
 *  another tenant cannot be mutated (the control plane 404s one under the org
 *  token; the demo tables have no such boundary unless we draw it). */
async function demoClusterInOrg(orgId: string, clusterId: string) {
  const [cluster] = await db
    .select()
    .from(s.clusters)
    .where(and(eq(s.clusters.id, clusterId), eq(s.clusters.orgId, orgId)));
  if (!cluster) throw new Error("Cluster not found.");
  return cluster;
}

export async function addClusterNode(input: {
  orgId: string;
  clusterId: string;
  serverId: string;
}): Promise<void> {
  const { user, role } = await requireProjectAdmin(input.orgId);
  if (cpEnabled()) {
    await cpAddClusterNode(input.orgId, input.clusterId, input.serverId, {
      name: user.name,
      role,
    });
  } else {
    const cluster = await demoClusterInOrg(input.orgId, input.clusterId);
    const server = await demoJoinableServer(input.orgId, input.serverId);
    await db.insert(s.clusterNodes).values({
      clusterId: cluster.id,
      serverId: server.id,
      role: NODE_ROLE_WORKER,
      joinedAt: new Date(),
    });
  }
  await writeAudit({
    orgId: input.orgId,
    actor: user.name,
    action: "Added cluster node",
    target: input.serverId,
  });
  revalidatePath("/dashboard", "layout");
}

export async function removeClusterNode(input: {
  orgId: string;
  clusterId: string;
  serverId: string;
}): Promise<void> {
  const { user, role } = await requireProjectAdmin(input.orgId);
  if (cpEnabled()) {
    await cpRemoveClusterNode(input.orgId, input.clusterId, input.serverId, {
      name: user.name,
      role,
    });
  } else {
    await demoClusterInOrg(input.orgId, input.clusterId);
    const [node] = await db
      .select()
      .from(s.clusterNodes)
      .where(
        and(
          eq(s.clusterNodes.clusterId, input.clusterId),
          eq(s.clusterNodes.serverId, input.serverId)
        )
      );
    if (!node) throw new Error("That server is not a node of this cluster.");
    // store.ErrControlPlaneNode, in the same words: draining the node that runs
    // the API server does not shrink the cluster, it ends it.
    if (node.role === NODE_ROLE_CONTROL_PLANE) {
      throw new Error("The control-plane node cannot be removed; delete the cluster instead.");
    }
    await db
      .delete(s.clusterNodes)
      .where(
        and(
          eq(s.clusterNodes.clusterId, input.clusterId),
          eq(s.clusterNodes.serverId, input.serverId)
        )
      );
  }
  await writeAudit({
    orgId: input.orgId,
    actor: user.name,
    action: "Removed cluster node",
    target: input.serverId,
  });
  revalidatePath("/dashboard", "layout");
}

export async function deleteCluster(input: {
  orgId: string;
  clusterId: string;
}): Promise<void> {
  const { user, role } = await requireProjectAdmin(input.orgId);
  let name = input.clusterId;
  if (cpEnabled()) {
    await cpDeleteCluster(input.orgId, input.clusterId, { name: user.name, role });
  } else {
    const cluster = await demoClusterInOrg(input.orgId, input.clusterId);
    name = cluster.name;
    // The workloads survive their target, which is what the delete dialog
    // promises: resources.cluster_id is ON DELETE SET NULL, so they are left
    // with no target and stop running rather than being deleted with it. Say
    // so in the audit — a resource that silently stops is the kind of thing an
    // operator finds out about from a customer.
    const orphaned = await db
      .select({ id: s.resources.id, name: s.resources.name })
      .from(s.resources)
      .where(eq(s.resources.clusterId, cluster.id));
    await db.delete(s.clusters).where(eq(s.clusters.id, cluster.id));
    if (orphaned.length > 0) {
      await db
        .update(s.resources)
        .set({ status: "stopped" })
        .where(
          inArray(
            s.resources.id,
            orphaned.map((r) => r.id)
          )
        );
      await writeAudit({
        orgId: input.orgId,
        actor: user.name,
        action: `Cluster deleted — ${orphaned.length} workload(s) lost their target and stopped`,
        target: orphaned
          .map((r) => r.name)
          .sort()
          .join(", "),
      });
    }
  }
  await writeAudit({
    orgId: input.orgId,
    actor: user.name,
    action: "Deleted cluster",
    target: name,
  });
  revalidatePath("/dashboard", "layout");
}
