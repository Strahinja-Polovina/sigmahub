import { orgs, projects, environments, servers, resources, members } from "./data";
import type {
  ServerType,
  ResourceKind,
  MetricPoint,
  LogLine,
  Deployment,
  DeployStatus,
} from "./types";

export * from "./types";
export { orgs, projects, environments, servers, resources, members };

// Canonical billing (SIGMA-A-1): flat EUR 5 per connected server / month,
// all-inclusive, single meter; Free tier up to 3 connected servers.
export const UNIT_PRICE = 5;
export const FREE_TIER_SERVERS = 3;
export const CURRENCY = "EUR";

export function getOrgs() {
  return orgs;
}
export function getOrg(id: string) {
  return orgs.find((o) => o.id === id) ?? orgs[0];
}
export function getProjects(orgId: string) {
  return projects.filter((p) => p.orgId === orgId);
}
export function getProject(id: string) {
  return projects.find((p) => p.id === id);
}
export function getEnvironments(projectId: string) {
  return environments.filter((e) => e.projectId === projectId);
}
export function getEnvironment(id: string) {
  return environments.find((e) => e.id === id);
}
export function getServers(orgId: string) {
  return servers.filter((s) => s.orgId === orgId);
}
export function getServer(id: string) {
  return servers.find((s) => s.id === id);
}
export function getResources(environmentId: string) {
  return resources.filter((r) => r.environmentId === environmentId);
}
export function getResourcesByProject(projectId: string) {
  return resources.filter((r) => r.projectId === projectId);
}
export function getResource(id: string) {
  return resources.find((r) => r.id === id);
}
export function getMembers(_orgId: string) {
  return members;
}

// Resource-type × server-type availability matrix (SIGMA-A-3 §2).
export const availabilityMatrix: Record<ServerType, ResourceKind[]> = {
  general: ["app", "postgres", "mysql", "mongo", "redis"],
  database: ["postgres", "mysql", "mongo", "redis"],
  storage: ["s3"],
  gpu: ["llm", "app"],
};
export function canHost(server: ServerType, kind: ResourceKind) {
  return availabilityMatrix[server].includes(kind);
}

// Deterministic hash so mock data is stable across SSR/CSR (no Math.random).
function seedOf(s: string) {
  let h = 0;
  for (const c of s) h = (h * 31 + c.charCodeAt(0)) % 997;
  return h;
}
function hex(s: string) {
  let h = 5381;
  for (const c of s) h = ((h << 5) + h + c.charCodeAt(0)) >>> 0;
  return h.toString(16).padStart(8, "0");
}

export function getMetrics(seedKey: string, points = 24): MetricPoint[] {
  const h = seedOf(seedKey);
  const base = (h % 35) + 20;
  return Array.from({ length: points }, (_, i) => ({
    t: `${String(i).padStart(2, "0")}:00`,
    cpu: Math.max(2, Math.round(base + 18 * Math.sin((i + h) / 3) + (i % 5) * 2)),
    mem: Math.max(10, Math.round(base + 12 * Math.cos((i + h) / 4) + 22)),
    net: Math.max(1, Math.round(30 + 25 * Math.sin((i + h) / 2))),
  }));
}

const LOG_MSGS = [
  "GET /health 200 3ms",
  "request completed 200 in 21ms",
  "worker picked up job #{n}",
  "cache hit ratio 0.94",
  "slow query 812ms on orders",
  "reconnecting to upstream",
  "deploy: health check passed",
  "GET /api/v1/orders 200 18ms",
  "background sync ok",
  "rate limit near threshold",
  "container started",
  "memory usage 78%",
];
const LOG_LEVELS: LogLine["level"][] = ["info", "info", "info", "warn", "info", "error", "info"];

export function getLogs(seedKey: string, n = 40): LogLine[] {
  const h = seedOf(seedKey);
  return Array.from({ length: n }, (_, i) => {
    const k = (h + i * 7) % LOG_MSGS.length;
    return {
      t: `12:${String((h + i) % 60).padStart(2, "0")}:${String((i * 13) % 60).padStart(2, "0")}`,
      level: LOG_LEVELS[(h + i) % LOG_LEVELS.length],
      msg: LOG_MSGS[k].replace("{n}", String(1000 + i)),
    };
  });
}

export function getDeployments(resourceId: string): Deployment[] {
  return Array.from({ length: 6 }, (_, i) => ({
    id: `dep_${resourceId}_${i}`,
    resourceId,
    sha: hex(resourceId + i).slice(0, 7),
    status: (i === 0 ? "running" : i === 3 ? "failed" : "success") as DeployStatus,
    startedAt: `2027-03-0${(i % 3) + 1}T1${i}:0${i % 6}:00Z`,
    durationSec: 40 + ((i * 13) % 120),
    author: ["mila", "nikola", "ana"][i % 3],
  }));
}

export function getBillingSummary(orgId: string) {
  const connected = getServers(orgId).filter((s) => s.status !== "provisioning").length;
  const isFree = connected <= FREE_TIER_SERVERS;
  const amount = isFree ? 0 : connected * UNIT_PRICE;
  return {
    connected,
    freeTier: FREE_TIER_SERVERS,
    unitPrice: UNIT_PRICE,
    currency: CURRENCY,
    amount,
    isFree,
  };
}
