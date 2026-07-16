import type { ResourceKind, ServerType } from "@/lib/mock";

// Nested deploy target (project → environment → servers) fed to the wizard.
export type DeployTargetServer = {
  id: string;
  name: string;
  type: string;
  provider: string;
  region: string;
};
export type DeployTarget = {
  id: string;
  name: string;
  environments: {
    id: string;
    name: string;
    servers: DeployTargetServer[];
  }[];
};

// Human-readable labels for resource kinds (matches the Overview page).
export const KIND_LABELS: Record<ResourceKind, string> = {
  app: "App",
  postgres: "PostgreSQL",
  mysql: "MySQL",
  mongo: "MongoDB",
  redis: "Redis",
  s3: "Object storage",
  llm: "LLM",
};

// Human-readable labels for server types.
export const SERVER_TYPE_LABELS: Record<ServerType, string> = {
  general: "General",
  database: "Database",
  storage: "Storage",
  gpu: "GPU",
};

export function formatDate(iso: string | Date) {
  return new Date(iso).toLocaleDateString("en-GB", {
    day: "numeric",
    month: "short",
    year: "numeric",
  });
}

export function formatDateTime(iso: string | Date) {
  return new Date(iso).toLocaleString("en-GB", {
    day: "numeric",
    month: "short",
    hour: "2-digit",
    minute: "2-digit",
  });
}

export function formatDuration(sec: number) {
  if (sec < 60) return `${sec}s`;
  const m = Math.floor(sec / 60);
  const s = sec % 60;
  return `${m}m ${String(s).padStart(2, "0")}s`;
}

// Deploy-status presentation, mirroring the Overview page palette.
export const DEPLOY_STATUS_META: Record<
  string,
  { label: string; text: string; dot: string }
> = {
  queued: { label: "Queued", text: "text-muted-foreground", dot: "bg-muted-foreground" },
  running: { label: "Running", text: "text-blue-700", dot: "bg-blue-500" },
  success: { label: "Success", text: "text-emerald-700", dot: "bg-emerald-500" },
  failed: { label: "Failed", text: "text-red-700", dot: "bg-red-500" },
  building: { label: "Building", text: "text-amber-700", dot: "bg-amber-500" },
  // CP deploy-pipeline statuses (P1-9).
  deploying: { label: "Deploying", text: "text-blue-700", dot: "bg-blue-500" },
  superseded: { label: "Superseded", text: "text-muted-foreground", dot: "bg-muted-foreground" },
  rolled_back: { label: "Rolled back", text: "text-muted-foreground", dot: "bg-muted-foreground" },
};
