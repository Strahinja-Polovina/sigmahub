// SigmaHub prototype — mock domain model (SIGMA-A-3 §2).
// Org → Project → Environment → Server → Resource.

export type ServerType =
  | "general"
  // A general-purpose host that happens to be virtualized. Same capabilities as
  // "general"; the distinction is disclosure (shared tenancy, burst CPU, no
  // nested virtualization), not what it can run.
  | "vps"
  | "storage"
  | "database"
  | "gpu"
  // A cluster member. Workloads arrive through the cluster's control plane, so
  // nothing is ever scheduled onto a node directly.
  | "k8s"
  // Compiles images and pushes them to a registry; runs no long-lived workloads.
  | "build";
export type ResourceKind =
  | "app"
  | "postgres"
  | "mysql"
  | "mongo"
  | "redis"
  | "s3"
  | "llm";
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
