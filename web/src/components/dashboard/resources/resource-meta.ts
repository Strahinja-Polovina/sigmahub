// Re-export shared constants and formatters for backwards-compatible imports.
export { KIND_LABELS, SERVER_TYPE_LABELS, DEPLOY_STATUS_META } from "@/lib/constants";
export { formatDate, formatDateTime, formatDuration } from "@/lib/formatters";

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
