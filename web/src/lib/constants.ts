import type { ResourceKind, ServerType } from "@/lib/mock";

/** Human-readable labels for resource kinds. */
export const KIND_LABELS: Record<ResourceKind, string> = {
  app: "App",
  postgres: "PostgreSQL",
  mysql: "MySQL",
  mongo: "MongoDB",
  redis: "Redis",
  s3: "Object storage",
  llm: "LLM",
};

/** Human-readable labels for server types. */
export const SERVER_TYPE_LABELS: Record<ServerType, string> = {
  general: "General",
  storage: "Storage",
  database: "Database",
  gpu: "GPU",
};

/** Canonical ordering of server types for UI filters/tabs. */
export const SERVER_TYPE_ORDER: ServerType[] = [
  "general",
  "database",
  "storage",
  "gpu",
];

/** Deploy-status presentation metadata. */
export const DEPLOY_STATUS_META: Record<
  string,
  { label: string; text: string; dot: string }
> = {
  queued: { label: "Queued", text: "text-muted-foreground", dot: "bg-muted-foreground" },
  running: { label: "Running", text: "text-blue-700", dot: "bg-blue-500" },
  success: { label: "Success", text: "text-emerald-700", dot: "bg-emerald-500" },
  failed: { label: "Failed", text: "text-red-700", dot: "bg-red-500" },
  building: { label: "Building", text: "text-amber-700", dot: "bg-amber-500" },
};
