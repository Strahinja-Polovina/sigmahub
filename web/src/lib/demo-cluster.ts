/**
 * What a Kubernetes cluster does in demo mode, decided in one testable place
 * (SIGMA-215).
 *
 * With no control plane there is no k3s, no join token and no node agent to
 * report back — so a demo cluster's whole life is a set of rules over two
 * timestamps: when the cluster was created, and when each node joined. That
 * choice is not a shortcut, it is the only one that survives a page reload. A
 * simulation driven by writes would need something to keep writing, and there
 * is nothing running between requests here; a simulation derived from the row's
 * own clock tells the same story every time it is read, from any tab, for as
 * long as the row lives.
 *
 * The vocabulary is the control plane's, not a demo dialect: node_status is
 * pending | ready | error and the cluster status is re-derived from the nodes
 * exactly as store.rederiveClusterStatusTx does it. The panel renders one shape
 * in both modes, so a demo-only status string would be a blank badge.
 */

import { SERVER_STATUS } from "./server-compat";

/**
 * How long a demo node spends installing Kubernetes before it reports ready.
 *
 * Six seconds, and the number is a UI decision rather than an approximation of
 * anything: a real k3s install takes minutes, which nobody evaluating the
 * product is going to sit through, and an instant `ready` would delete the
 * state the panel exists to show — "provisioning", the joining badge, the
 * control plane coming up before its workers. Six is long enough to read the
 * cluster card and watch it change, short enough that nobody concludes it hung.
 */
export const DEMO_NODE_READY_MS = 6_000;

/** The k3s release a demo cluster reports. A version string that is not a real
 *  release would be the one detail in this panel that cannot be checked. */
export const DEMO_KUBERNETES_VERSION = "v1.31.4+k3s1";

/** The port a k3s API server listens on. The endpoint is built from the control
 *  plane node's MESH address: the API is never published on a public interface,
 *  which is the sentence the create dialog makes and which a demo endpoint of
 *  the host's public IP would quietly contradict. */
const K8S_API_PORT = 6443;

export const NODE_ROLE_CONTROL_PLANE = "control-plane";
export const NODE_ROLE_WORKER = "worker";

/** Node vocabulary, mirroring store.ClusterNodeReport. */
export const NODE_STATUS = {
  pending: "pending",
  ready: "ready",
  error: "error",
} as const;

/** Cluster vocabulary, mirroring the three states rederiveClusterStatusTx
 *  produces. Nothing else may be written to a cluster row's status. */
export const CLUSTER_STATUS = {
  provisioning: "provisioning",
  ready: "ready",
  degraded: "degraded",
} as const;

function ms(value: Date | string | null | undefined): number | null {
  if (!value) return null;
  const d = value instanceof Date ? value : new Date(value);
  const t = d.getTime();
  return Number.isNaN(t) ? null : t;
}

export type DemoNodeInput = {
  /** When the node joined — the cluster's own creation time for the control
   *  plane, since promoting a server IS its join. */
  joinedAt: Date | string | null | undefined;
  /** The server's status, as the enrollment gate left it. */
  serverStatus: string;
  now?: number;
};

export type DemoNodeReport = {
  status: string;
  /** Set only for `error`: the panel renders it under the node's name, so it
   *  has to say what went wrong on THAT host rather than restate the badge. */
  message: string;
};

/**
 * What a demo node reports about Kubernetes on it.
 *
 * A host the agent has not heard from cannot have installed anything, and that
 * is the case worth simulating rather than skipping: a cluster whose worker
 * went unreachable is `degraded`, and a demo that always reported `ready` could
 * never show the state the panel's amber node message was written for. So the
 * server's own status is consulted first, and only a host that is actually
 * running goes on to the clock.
 */
export function demoNodeReport(input: DemoNodeInput): DemoNodeReport {
  if (input.serverStatus === SERVER_STATUS.unreachable) {
    return {
      status: NODE_STATUS.error,
      message: "The host stopped answering, so this node has dropped out of the cluster.",
    };
  }
  if (input.serverStatus === SERVER_STATUS.incompatible) {
    return {
      status: NODE_STATUS.error,
      message:
        "This host was refused for its server type, so Kubernetes was never installed on it.",
    };
  }
  if (input.serverStatus === SERVER_STATUS.decommissioning) {
    return {
      status: NODE_STATUS.error,
      message: "This host is being decommissioned and is leaving the cluster.",
    };
  }
  if (input.serverStatus !== SERVER_STATUS.running) {
    // Still provisioning: the agent has not checked in, so nothing has begun.
    return { status: NODE_STATUS.pending, message: "" };
  }
  const joined = ms(input.joinedAt);
  const now = input.now ?? Date.now();
  if (joined === null || now - joined >= DEMO_NODE_READY_MS) {
    return { status: NODE_STATUS.ready, message: "" };
  }
  return { status: NODE_STATUS.pending, message: "" };
}

/**
 * How long until this node would report ready, or null when there is nothing to
 * wait for.
 *
 * The panel is a server component's output, so a node that goes ready six
 * seconds after it joined goes ready on the NEXT render — and nothing was
 * asking for one. A person who presses "Create cluster" and watches sees
 * "provisioning" until they think to reload, which is the infinite spinner this
 * whole programme exists to delete, wearing a badge.
 *
 * Null covers three unrelated things on purpose — already ready, never going to
 * be (the host is not running), and no timestamp at all — because the caller
 * does the same thing with all three: it does not schedule a refresh. A node
 * stuck on an unreachable host recovers when the host does, and that is an
 * event, not a deadline.
 */
export function msUntilNodeReady(input: DemoNodeInput): number | null {
  if (input.serverStatus !== SERVER_STATUS.running) return null;
  const joined = ms(input.joinedAt);
  if (joined === null) return null;
  const elapsed = (input.now ?? Date.now()) - joined;
  return elapsed >= DEMO_NODE_READY_MS ? null : DEMO_NODE_READY_MS - elapsed;
}

/**
 * A cluster's status from its nodes' — the TypeScript half of
 * store.rederiveClusterStatusTx, deliberately spelled the same way:
 *
 *   ready        the control plane is up and every node reported ready
 *   degraded     the control plane is up and some node did not
 *   provisioning the control plane has not reported ready yet
 *
 * A cluster with no nodes at all is provisioning rather than ready. It cannot
 * happen through the actions — deleting the control-plane node is refused and
 * deleting the cluster removes the row — but "every node is ready" is
 * vacuously true of an empty list, and a cluster that reported `ready` with
 * nothing in it would be the one status in this panel that means nothing.
 */
export function demoClusterStatus(
  nodes: { role: string; status: string }[]
): string {
  const controlPlaneReady = nodes.some(
    (n) => n.role === NODE_ROLE_CONTROL_PLANE && n.status === NODE_STATUS.ready
  );
  if (!controlPlaneReady) return CLUSTER_STATUS.provisioning;
  return nodes.every((n) => n.status === NODE_STATUS.ready)
    ? CLUSTER_STATUS.ready
    : CLUSTER_STATUS.degraded;
}

/** The API endpoint a demo cluster publishes, or "" while the control-plane
 *  node has no mesh address yet — an empty string is what the card checks, and
 *  a placeholder URL there would be an address someone could try to curl. */
export function demoApiEndpoint(controlPlaneMeshIp: string | null | undefined): string {
  const ip = (controlPlaneMeshIp ?? "").trim();
  return ip ? `https://${ip}:${K8S_API_PORT}` : "";
}

/** The version a cluster reports, which is nothing until its control plane is
 *  actually serving. The card prints this verbatim next to the node count. */
export function demoKubernetesVersion(clusterStatus: string): string {
  return clusterStatus === CLUSTER_STATUS.provisioning ? "" : DEMO_KUBERNETES_VERSION;
}

/**
 * Why this server cannot be promoted into a cluster, or null when it can.
 *
 * The control plane's own CreateCluster only checks tenancy — it takes any
 * server in the org — because in CP mode the k3s install is what fails, loudly,
 * on a host that cannot run it. Demo mode has no install to fail, so the
 * refusal has to be made here or a demo user builds a cluster on a host the
 * product would not have accepted and learns the wrong rule. The sentences name
 * the fix, because every one of these states has one.
 */
export function controlPlaneRefusal(server: {
  name: string;
  status: string;
}): string | null {
  switch (server.status) {
    case SERVER_STATUS.running:
      return null;
    case SERVER_STATUS.provisioning:
      return `${server.name} has not checked in yet. Kubernetes is installed by the agent, so wait for it to connect.`;
    case SERVER_STATUS.unreachable:
      return `${server.name} has stopped answering. Bring it back before promoting it to a control plane.`;
    case SERVER_STATUS.incompatible:
      return `${server.name} was refused for its server type. Change its type or fix the host first.`;
    case SERVER_STATUS.decommissioning:
      return `${server.name} is being disconnected, so it cannot take on new work.`;
    default:
      return `${server.name} is not ready to run a cluster control plane.`;
  }
}
