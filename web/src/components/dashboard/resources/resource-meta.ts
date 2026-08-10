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

/** The ONE place a server row becomes a wizard deploy target.
 *
 *  There are two builders — getDeployTargets for the Resources page and the
 *  project page's own per-environment target — and they drifted: the project
 *  page's inline mapping carried id/name/type/provider/region/status and not
 *  `gpu`, so the SIGMA-214 VRAM fit check read every target opened from a
 *  project as UNKNOWN and warned about nothing. The user picked a 70B model
 *  onto a 24 GB box, the control plane's create-time checkModelFits refused it
 *  with a 422, and the wizard's failure path threw away six steps of input
 *  (SIGMA-304). Both builders call this now so the shapes cannot diverge again.
 *
 *  Only the fields a TARGET decision needs are copied — the rest of a host's
 *  facts (kernel, disks, docker version) is weight on a payload nothing reads. */
export function toDeployTargetServer(sv: {
  id: string;
  name: string;
  type: string;
  provider?: string | null;
  region?: string | null;
  status?: string | null;
  facts?: HostFacts | null;
}): DeployTargetServer {
  return {
    id: sv.id,
    name: sv.name,
    type: sv.type,
    provider: sv.provider ?? "",
    region: sv.region ?? "",
    // The enrollment gate's verdict, so the wizard can refuse a host the
    // control plane already refused instead of letting create say no first
    // (SIGMA-203).
    status: sv.status ?? undefined,
    // Undefined when the agent reported no inventory, which the fit check
    // reads as UNKNOWN and never as "no GPU".
    gpu: sv.facts?.gpu ?? undefined,
  };
}

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

/**
 * The deploy statuses that mean "work is happening right now".
 *
 * One list, because the page has to agree with itself about this: a resource
 * with an in-flight deployment must poll for fresh data AND suppress the
 * previous failure's banner. Those used to be two separate judgements, and the
 * disagreement was visible — the page showed "This resource is failing" with
 * the last run's error for the entire two-and-a-half minutes a new build was
 * running, because the banner read the last APPLIED status while the pipeline
 * had already moved on.
 *
 * `running` is here for demo mode, whose simulated pipeline uses it where the
 * control plane says `deploying`.
 */
export const DEPLOY_IN_FLIGHT: ReadonlySet<string> = new Set([
  "queued",
  "building",
  "deploying",
  "running",
]);

export function isDeployInFlight(status: string): boolean {
  return DEPLOY_IN_FLIGHT.has(status);
}
