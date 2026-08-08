"use server";

import { requireResourceVisible } from "../active-org";
import { cpEnabled, cpDomainDNS, type CpDNSSetup } from "../cp";

/**
 * The DNS records a custom domain needs, plus a live check.
 *
 * A CP/DNS failure returns an explicitly-unverified result rather than
 * throwing: Next.js redacts thrown server-action messages in production, so a
 * throw would reach the user as an opaque digest instead of "we couldn't check
 * right now" — which is exactly the information this panel exists to give.
 */
export async function getDomainDNS(input: {
  orgId: string;
  resourceId: string;
  domainId: string;
}): Promise<CpDNSSetup | null> {
  if (!cpEnabled()) return null;
  await requireResourceVisible(input.orgId, input.resourceId);
  try {
    return await cpDomainDNS(input.orgId, input.domainId);
  } catch {
    return null;
  }
}
