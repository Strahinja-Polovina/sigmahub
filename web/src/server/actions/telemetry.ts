"use server";

import {
  requireMembership,
  requireResourceVisible,
  requireEnvironmentVisible,
  hasFullOrgVisibility,
} from "../active-org";
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
  // P2-7 read scoping (SIGMA-84): a project-scoped user may only search logs of
  // a resource/environment they can see. An unscoped (org-wide) search is
  // allowed only for users who can see the whole org.
  if (input.resourceId) {
    await requireResourceVisible(input.orgId, input.resourceId);
  } else if (input.environmentId) {
    await requireEnvironmentVisible(input.orgId, input.environmentId);
  } else if (!(await hasFullOrgVisibility(input.orgId))) {
    throw new Error("Specify an environment or resource to search.");
  }
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
