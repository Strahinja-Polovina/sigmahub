"use server";

import { requireMembership } from "../active-org";
import {
  cpEnabled,
  cpQueryLogs,
  cpBetaMetrics,
  type CpLogLine,
  type CpBetaMetrics,
} from "../cp";

/** Env-wide log search (P1-13): one query bar over every resource in an
 *  environment. Filters are allowlisted parameters; the CP builds the LogQL
 *  selector and Loki enforces the org tenant. Returns null when the pipeline
 *  is not configured. */
export async function searchLogs(input: {
  orgId: string;
  environmentId?: string;
  resourceId?: string;
  q?: string;
  limit?: number;
}): Promise<CpLogLine[] | null> {
  if (!cpEnabled()) return null;
  await requireMembership(input.orgId);
  return cpQueryLogs(input.orgId, {
    environmentId: input.environmentId,
    resourceId: input.resourceId,
    q: input.q,
    limit: input.limit ?? 200,
  });
}

/** The M1 exit-criteria feed for the beta-metrics settings tab. */
export async function getBetaMetrics(input: { orgId: string }): Promise<CpBetaMetrics | null> {
  if (!cpEnabled()) return null;
  await requireMembership(input.orgId);
  return cpBetaMetrics(input.orgId);
}
