import type { Status } from "@/lib/mock";

/** The control plane and the agent speak their own state vocabulary — the agent
 *  emits `applied|failed|skipped`, the CP adds `unreachable` for a stale server —
 *  none of which are UI `Status` values.
 *
 *  This map is the SINGLE translation point. It used to live inside the status
 *  badge, so it applied at render time only: the mirror stored raw CP states,
 *  and every aggregate that compared against UI vocabulary silently missed —
 *  "Running resources: 0" next to a list of green Running badges, and project
 *  cards reading "No resources yet" with a non-zero resource count (SIGMA-176).
 *  Translating at the boundary instead keeps stored state and rendered state in
 *  one vocabulary. */
export const STATE_ALIASES: Record<string, Status> = {
  // Converged / healthy.
  applied: "running",
  ready: "running",
  active: "running",
  // In flight.
  pending: "provisioning",
  creating: "provisioning",
  deploying: "provisioning",
  building: "provisioning",
  // Broken. `skipped` is what the agent journals for a dependent of a failed op
  // (a rollout whose clone/build failed), so it means "did not deploy", not
  // "unknown" (SIGMA-189).
  failed: "error",
  skipped: "error",
  // A server the CP's staleness sweep flipped after 90s of silence. Without
  // this it rendered as a grey "Unknown" pill (SIGMA-184).
  unreachable: "stopped",
};

/** Normalize any CP/agent state string (or a `{state}` object) to UI vocabulary.
 *  Returns null when the value is empty or unrecognized, so callers can decide
 *  between keeping a previous value and showing a neutral fallback. */
export function normalizeStatus(status: unknown): Status | null {
  let key = "";
  if (typeof status === "string") key = status;
  else if (status && typeof status === "object" && "state" in status) {
    key = String((status as { state: unknown }).state ?? "");
  }
  if (!key) return null;
  const mapped = STATE_ALIASES[key];
  if (mapped) return mapped;
  // Already UI vocabulary (the demo path stores these directly).
  if (key === "running" || key === "degraded" || key === "stopped" || key === "provisioning" || key === "error") {
    return key;
  }
  return null;
}
