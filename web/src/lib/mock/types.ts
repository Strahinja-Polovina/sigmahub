// SigmaHub prototype — mock domain model (SIGMA-A-3 §2).
// Org → Project → Environment → Server → Resource.

// The server-type and resource-kind vocabularies are the control plane's, not
// ours: they are generated from cp/internal/store/server_catalog.go. Re-exported
// here so the hundreds of `@/lib/mock` imports keep working while there is only
// one definition (SIGMA-198 — this file used to hold a hand-maintained copy that
// spelled MongoDB "mongo" while the CP, the agent and every migration said
// "mongodb").
import type { ServerType, ResourceKind } from "@/lib/server-catalog.generated";

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

export interface Server {
  id: string;
  orgId: string;
  name: string;
  type: ServerType;
  provider: string;
  region: string;
  status: Status;
  agentVersion: string;
  ip: string;
  cpu: number;
  memGb: number;
  connectedAt: string;
  environmentIds: string[];
  resourceCount: number;
  byoVpn: boolean; // connected over a customer-provided VPN / jump host
}

export interface Resource {
  id: string;
  projectId: string;
  environmentId: string;
  serverId: string;
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
