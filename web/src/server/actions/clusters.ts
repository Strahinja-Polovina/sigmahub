"use server";

import { revalidatePath } from "next/cache";
import {
  requireMembership,
  requireProjectAdmin,
  requireEnvironmentVisible,
} from "../active-org";
import { writeAudit } from "../audit";
import {
  cpEnabled,
  cpListClusters,
  cpCreateCluster,
  cpAddClusterNode,
  cpRemoveClusterNode,
  cpDeleteCluster,
  type CpCluster,
} from "../cp";

function ensureCp() {
  if (!cpEnabled()) {
    throw new Error("Kubernetes clusters require the control plane (set SIGMAHUB_CP_URL).");
  }
}

/** The org's clusters, plus which resource kinds a cluster refuses. */
export async function listClusters(
  orgId: string,
  environmentId?: string
): Promise<{ clusters: CpCluster[]; excludedKinds: string[] }> {
  ensureCp();
  await requireMembership(orgId);
  try {
    return await cpListClusters(orgId, environmentId);
  } catch {
    return { clusters: [], excludedKinds: [] };
  }
}

/** Build a cluster, promoting the chosen server into its control plane. */
export async function createCluster(input: {
  orgId: string;
  environmentId: string;
  name: string;
  controlPlaneId: string;
}): Promise<CpCluster> {
  ensureCp();
  await requireEnvironmentVisible(input.orgId, input.environmentId);
  const { user, role } = await requireProjectAdmin(input.orgId);
  const cluster = await cpCreateCluster(
    input.orgId,
    {
      environmentId: input.environmentId,
      name: input.name.trim(),
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
  revalidatePath("/dashboard/servers");
  return cluster;
}

export async function addClusterNode(input: {
  orgId: string;
  clusterId: string;
  serverId: string;
}): Promise<void> {
  ensureCp();
  const { user, role } = await requireProjectAdmin(input.orgId);
  await cpAddClusterNode(input.orgId, input.clusterId, input.serverId, {
    name: user.name,
    role,
  });
  await writeAudit({
    orgId: input.orgId,
    actor: user.name,
    action: "Added cluster node",
    target: input.serverId,
  });
  revalidatePath("/dashboard/servers");
}

export async function removeClusterNode(input: {
  orgId: string;
  clusterId: string;
  serverId: string;
}): Promise<void> {
  ensureCp();
  const { user, role } = await requireProjectAdmin(input.orgId);
  await cpRemoveClusterNode(input.orgId, input.clusterId, input.serverId, {
    name: user.name,
    role,
  });
  await writeAudit({
    orgId: input.orgId,
    actor: user.name,
    action: "Removed cluster node",
    target: input.serverId,
  });
  revalidatePath("/dashboard/servers");
}

export async function deleteCluster(input: {
  orgId: string;
  clusterId: string;
}): Promise<void> {
  ensureCp();
  const { user, role } = await requireProjectAdmin(input.orgId);
  await cpDeleteCluster(input.orgId, input.clusterId, { name: user.name, role });
  await writeAudit({
    orgId: input.orgId,
    actor: user.name,
    action: "Deleted cluster",
    target: input.clusterId,
  });
  revalidatePath("/dashboard/servers");
}
