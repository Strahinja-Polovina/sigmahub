import { Badge } from "@/components/ui/badge";
import type { ResourceKind, ServerType } from "@/lib/mock";

// Human labels for resource kinds — mirrors the Overview page for consistency.
export const KIND_LABELS: Record<ResourceKind, string> = {
  app: "App",
  postgres: "PostgreSQL",
  mysql: "MySQL",
  mongo: "MongoDB",
  redis: "Redis",
  s3: "Object storage",
  llm: "LLM",
};

export const SERVER_TYPE_LABELS: Record<ServerType, string> = {
  general: "General",
  storage: "Storage",
  database: "Database",
  gpu: "GPU",
};

export function KindBadge({ kind }: { kind: ResourceKind }) {
  return (
    <Badge variant="outline" className="font-mono">
      {KIND_LABELS[kind]}
    </Badge>
  );
}

export function ServerTypeBadge({ type }: { type: ServerType }) {
  return (
    <Badge variant="secondary" className="font-normal">
      {SERVER_TYPE_LABELS[type]}
    </Badge>
  );
}

export function formatDate(iso: string) {
  return new Date(iso).toLocaleDateString("en-GB", {
    day: "numeric",
    month: "short",
    year: "numeric",
  });
}

export function formatDateTime(iso: string) {
  return new Date(iso).toLocaleString("en-GB", {
    day: "numeric",
    month: "short",
    hour: "2-digit",
    minute: "2-digit",
  });
}
