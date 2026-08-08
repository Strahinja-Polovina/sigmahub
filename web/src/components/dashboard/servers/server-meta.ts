import type { ServerType, ResourceKind } from "@/lib/mock";

// Human labels + tone for each server type. Kept monochrome/outline in the UI —
// the type is meta, not status, so we do not color it like a status pill.
export const SERVER_TYPE_LABELS: Record<ServerType, string> = {
  general: "General",
  vps: "VPS",
  storage: "Storage",
  database: "Database",
  gpu: "GPU",
  k8s: "Cluster node",
  build: "Build",
};

export const RESOURCE_KIND_LABELS: Record<ResourceKind, string> = {
  app: "App",
  postgres: "PostgreSQL",
  mysql: "MySQL",
  mongo: "MongoDB",
  redis: "Redis",
  s3: "Object storage",
  llm: "LLM",
};

// Order used for the type filter and grouping on the list page.
export const SERVER_TYPE_ORDER: ServerType[] = [
  "general",
  "vps",
  "database",
  "storage",
  "gpu",
  "k8s",
  "build",
];

// Types offered when connecting a NEW server. "k8s" is absent on purpose: a
// node becomes one by joining a cluster, never by being declared one at
// enrollment, so offering it here would create a host that hosts nothing.
export const CONNECTABLE_SERVER_TYPES: ServerType[] = [
  "general",
  "vps",
  "database",
  "storage",
  "gpu",
  "build",
];

/** One line explaining what each type is for, shown in the connect dialog. */
export const SERVER_TYPE_HINTS: Partial<Record<ServerType, string>> = {
  general: "Apps and databases on a machine you control end to end.",
  vps: "A virtualized host — same capabilities, shared tenancy and burst CPU.",
  database: "Tuned for managed database engines with production-grade settings.",
  storage: "Large disks for S3-compatible object storage.",
  gpu: "NVIDIA hardware for model hosting; drivers and the runtime are managed.",
  k8s: "Joined to a cluster — workloads arrive through its control plane.",
  build: "Compiles images for other servers; runs no long-lived workloads.",
};

export function formatDate(iso: string | Date) {
  return new Date(iso).toLocaleDateString("en-GB", {
    day: "numeric",
    month: "short",
    year: "numeric",
  });
}
