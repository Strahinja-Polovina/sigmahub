import "server-only";
import { cpEnabled, cpQueryLogs, cpQueryResourceMetrics } from "./cp";
import type { CpTelemetry } from "@/components/dashboard/resources/resource-detail";

/**
 * Load real pipeline telemetry for a resource (P1-13, CP mode only).
 *
 * This lives beside the other control-plane readers rather than inside the page
 * because of what its error path has to do. Every other loader on the resource
 * page goes through `attempt()` and records the read it could not perform, so
 * the page can say "the control plane didn't answer for …". This one alone
 * swallowed the failure and returned `{ pipeline: true, metrics: [], logs: [] }`
 * — which the UI renders as "No telemetry received yet", i.e. the pipeline IS
 * configured and the container produced nothing.
 *
 * That is the opposite of the truth, and it is the sentence an operator reads
 * while their app is crash-looping and the control plane is mid-restart: they
 * conclude the container never started, and go hunting on the host for output
 * the page never asked for (SIGMA-236). A failed read now both records itself
 * in `failures` and marks the payload `unreadable`, so the three states —
 * not configured, configured-but-empty, could-not-ask — stay distinct.
 *
 * @param failures collected read failures; appended to, never read.
 * @returns null in demo mode (there is no control plane to ask).
 */
export async function loadResourceTelemetry(
  orgId: string,
  resourceId: string,
  failures: string[]
): Promise<CpTelemetry | null> {
  if (!cpEnabled()) return null;
  try {
    const [metrics, logs] = await Promise.all([
      cpQueryResourceMetrics(orgId, resourceId),
      cpQueryLogs(orgId, { resourceId, limit: 200 }),
    ]);
    // null from either query means that half of the pipeline is not configured;
    // an empty array means it is configured and has nothing yet.
    return {
      pipeline: metrics !== null || logs !== null,
      metrics: metrics ?? [],
      logs: logs ?? [],
    };
  } catch {
    failures.push("logs and metrics");
    return { pipeline: true, unreadable: true, metrics: [], logs: [] };
  }
}
