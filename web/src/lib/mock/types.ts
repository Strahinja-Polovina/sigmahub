// SigmaHub prototype — mock domain model (SIGMA-A-3 §2).
// Org → Project → Environment → Server → Resource.

// The server-type and resource-kind vocabularies are the control plane's, not
// ours: they are generated from cp/internal/store/server_catalog.go. Re-exported
// here so the hundreds of `@/lib/mock` imports keep working while there is only
// one definition (SIGMA-198 — this file used to hold a hand-maintained copy that
// spelled MongoDB "mongo" while the CP, the agent and every migration said
// "mongodb").
import type { ServerType, ResourceKind } from "@/lib/server-catalog.generated";
import type { HostFacts } from "@/lib/server-compat";

export type { ServerType, ResourceKind };
export type Status =
  | "running"
  | "degraded"
  | "stopped"
  | "provisioning"
  | "error";
export type DeployStatus = "success" | "failed" | "building" | "running";
export type Role = "Org Admin" | "Project Admin" | "Developer";

export interface Org {
  id: string;
  name: string;
  slug: string;
  plan: "free" | "cloud";
  memberCount: number;
  serverCount: number;
}

export interface Project {
  id: string;
  orgId: string;
  name: string;
  slug: string;
  description: string;
  environmentIds: string[];
}

export interface Environment {
  id: string;
  projectId: string;
  name: string; // free-text: production / staging / dev / ...
  serverIds: string[];
}

/** One server's membership in a cluster (SIGMA-206).
 *
 *  Three fields, where store.ClusterNode has eight, and the five that are gone
 *  are gone because they are ANSWERS rather than facts: what a node reports
 *  about k3s on it, and when, is computed from the host's own status and how
 *  long ago it joined (demoNodeReport), on every read. A fixture that wrote
 *  `nodeStatus: "error"` here would be overwritten the first time the panel
 *  rendered, which is a fixture that lies in a way nobody would ever see. */
export interface ClusterNode {
  serverId: string;
  role: "control-plane" | "worker";
  /** How long ago the server joined, in days — an OFFSET for the same reason
   *  the decommission clock is one: a node reports ready only once it has been
   *  in the cluster long enough, so a calendar date decides that for us, and a
   *  date in this fixture's own 2027 series decides it wrong. */
  joinedDaysAgo: number;
}

/**
 * A Kubernetes cluster over the org's own servers.
 *
 * No status, no API endpoint and no version, for the same reason: the control
 * plane DERIVES a cluster's status from its node rows and nothing else writes
 * the column ("the derivation is the definition" —
 * store.rederiveClusterStatusTx), and the endpoint and version follow from that
 * status and the control-plane node's mesh address. The seed runs the demo's
 * own derivations over these rows, so the seeded cluster is the cluster the
 * dashboard would have built from the same servers.
 */
export interface Cluster {
  id: string;
  orgId: string;
  environmentId: string;
  name: string;
  createdBy: string;
  createdDaysAgo: number;
  nodes: ClusterNode[];
}

export interface Server {
  id: string;
  orgId: string;
  name: string;
  type: ServerType;
  provider: string;
  region: string;
  /** The status the host's own behaviour justifies. It is not the last word:
   *  the seed re-derives `incompatible` from the facts and `decommissioning`
   *  from the teardown clock below, both the way the product does. */
  status: Status;
  agentVersion: string;
  /** The host's PUBLIC address — what the operator typed into the connect form
   *  and where the install command ran. Documentation-range addresses
   *  (RFC 5737) throughout: a demo dataset must not print somebody's real
   *  machine at a prospective customer. */
  ip: string;
  /** The private 10.8.x.x WireGuard address the agent is given at check-in.
   *  Kept apart from `ip` because they answer different questions and the
   *  dashboard once showed the second under the first's heading (SIGMA-187) —
   *  and because a cluster's API endpoint is a mesh address, so the demo needs
   *  both to be true at once. Empty for a host that has never checked in. */
  meshIp: string;
  cpu: number;
  memGb: number;
  connectedAt: string;
  environmentIds: string[];
  resourceCount: number;
  byoVpn: boolean; // connected over a customer-provided VPN / jump host
  /** What the agent reported about the host (SIGMA-201). Absent means no agent
   *  has ever checked in — a server still provisioning — and never "a host with
   *  nothing on it". Where it IS set, the seed runs the real compatibility gate
   *  over it rather than hand-writing a status, so a demo server enrolled as the
   *  wrong type ends up incompatible for the same reason a real one would, with
   *  the same sentence (SIGMA-203). */
  facts?: HostFacts;
  /** A graceful decommission in flight (SIGMA-204), as an OFFSET rather than a
   *  date: the dialog, its "Force disconnect" affordance and its timeout are all
   *  comparisons against the clock, so a calendar date in a fixture would decide
   *  which of those the demo shows by the accident of when it is opened. The
   *  seed turns this into `now − startedMinutesAgo`; see the fixture for the
   *  offset chosen and why. */
  decommission?: { startedMinutesAgo: number; purgeVolumes: boolean };
}

export interface Resource {
  id: string;
  projectId: string;
  environmentId: string;
  /** Exactly one of serverId and clusterId is set — the same rule the control
   *  plane enforces with a CHECK constraint (migration 0050). A workload the
   *  scheduler places has no server of its own, and a resource with neither
   *  target is one nothing renders. */
  serverId: string | null;
  clusterId?: string;
  name: string;
  kind: ResourceKind;
  status: Status;
  lastDeployAt: string;
  repo?: string;
  image?: string;
  domain?: string;
  version?: string;
}

export interface Deployment {
  id: string;
  resourceId: string;
  sha: string;
  status: DeployStatus;
  startedAt: string;
  durationSec: number;
  author: string;
}

export interface MetricPoint {
  t: string;
  cpu: number;
  mem: number;
  net: number;
}

export interface LogLine {
  t: string;
  level: "info" | "warn" | "error";
  msg: string;
}

export interface Member {
  id: string;
  name: string;
  email: string;
  role: Role;
}
