import type { HostFacts } from "@/lib/server-compat";

// Nested deploy target (project → environment → servers) fed to the wizard.
export type DeployTargetServer = {
  id: string;
  name: string;
  type: string;
  provider: string;
  region: string;
  /** The enrollment gate's verdict. A host it refused matches the availability
   *  matrix on paper and not in fact, and the control plane refuses to schedule
   *  onto it — so the wizard has to stop offering it as a target rather than
   *  let the operator find out at create (SIGMA-203). */
  status?: string;
  /** The host's GPU inventory — the ONLY slice of its facts a target decision
   *  needs, and the one the VRAM fit check compares the chosen model against
   *  (SIGMA-214). Absent means no agent ever reported one, which is unknown
   *  rather than none, and unknown never filters a server out. */
  gpu?: HostFacts["gpu"];
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

// Labels come from the control plane's catalog (generated, SIGMA-198). This
// module used to keep its own copy — one of four that had to be edited in
// lockstep to add a resource kind, and the reason MongoDB was labelled under two
// different keys depending on which screen you were looking at.
export {
  RESOURCE_KIND_LABELS as KIND_LABELS,
  SERVER_TYPE_LABELS,
} from "@/lib/server-catalog.generated";

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
