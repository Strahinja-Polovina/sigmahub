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
  // A server whose agent is tearing the host down (SIGMA-204). It maps to
  // `provisioning` — the in-flight bucket — because that is what it is: a
  // transition with a known end, and the only one of these states where a
  // spinner is honest. It keeps its own LABEL (see RAW_LABELS): "Provisioning"
  // on a machine being removed would read as the exact opposite of what is
  // happening. Every aggregate that counts running servers excludes it, which
  // is what stops a decommissioning host being counted as fleet capacity.
  decommissioning: "provisioning",
  // A host whose facts do not satisfy the type it was enrolled as (SIGMA-203).
  // It maps to `error` because it needs the operator to act — the two exits are
  // changing the type or disconnecting — and it keeps its own LABEL in the
  // badge (see RAW_LABELS): "Error" would send someone looking for a crash,
  // and every aggregate that counts running servers must exclude it, which
  // mapping to `provisioning` would not have done.
  incompatible: "error",
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

/** Raw CP/agent states that share a UI Status but deserve their own wording.
 *
 *  It lives here, next to STATE_ALIASES, because the two decisions are halves
 *  of one translation: the alias says which bucket a state falls in (and so how
 *  every aggregate counts it), the label says what the operator reads. Keeping
 *  the label in the badge component made the second half untestable and let it
 *  drift — a state could be aliased into a bucket and then rendered with that
 *  bucket's word, which is how "Decommissioning" would have shown up as
 *  "Provisioning" on a machine being removed. */
export const RAW_STATUS_LABELS: Record<string, string> = {
  // Red like `stopped`, but "Stopped" implies someone stopped it; this means
  // the agent went silent (SIGMA-184).
  unreachable: "Unreachable",
  // The deploy never ran because a prerequisite failed (SIGMA-189).
  skipped: "Not deployed",
  // The host installed and is heartbeating; what is wrong is the TYPE it was
  // enrolled as (SIGMA-203). "Error" would send the operator hunting for a
  // crash instead of reading the sentence next to this badge.
  incompatible: "Incompatible",
  // The agent is removing the workloads and then itself (SIGMA-204). In the
  // in-flight bucket, because that is what it is — but "Provisioning" on a
  // machine being decommissioned reads as the exact opposite of the truth.
  decommissioning: "Decommissioning",
};
