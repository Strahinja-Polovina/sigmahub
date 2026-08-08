import type { ServerType, ResourceKind } from "@/lib/mock";

// Human labels + tone for each server type. Kept monochrome/outline in the UI —
// the type is meta, not status, so we do not color it like a status pill.
export const SERVER_TYPE_LABELS: Record<ServerType, string> = {
  general: "General",
  storage: "Storage",
  database: "Database",
  gpu: "GPU",
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
  "database",
  "storage",
  "gpu",
];

// Types offered when connecting a NEW server. GPU hosting isn't onboardable
// yet, but stays in SERVER_TYPE_ORDER so existing gpu servers still render.
export const CONNECTABLE_SERVER_TYPES: ServerType[] = [
  "general",
  "database",
  "storage",
];

export function formatDate(iso: string | Date) {
  return new Date(iso).toLocaleDateString("en-GB", {
    day: "numeric",
    month: "short",
    year: "numeric",
  });
}
