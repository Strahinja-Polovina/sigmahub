import { notFound, redirect } from "next/navigation";
import { getActiveOrgId, requireMembership } from "@/server/active-org";
import { getResourceDetail } from "@/server/queries";
import { effectiveSecrets } from "@/server/secrets-data";
import { cpEnabled, cpListDomains } from "@/server/cp";
import { ResourceDetail } from "@/components/dashboard/resources/resource-detail";
import type { DomainRow } from "@/components/dashboard/resources/resource-domains-panel";

/** Load an app resource's custom domains (CP mode only). A CP failure degrades
 *  to an empty list rather than breaking the page. */
async function loadDomains(orgId: string, resourceId: string, kind: string): Promise<DomainRow[]> {
  if (!cpEnabled() || kind !== "app") return [];
  try {
    return (await cpListDomains(orgId, resourceId)).map((d) => ({
      id: d.id,
      domain: d.domain,
      certStatus: d.certStatus,
      certExpiresAt: d.certExpiresAt,
      lastError: d.lastError,
    }));
  } catch {
    return [];
  }
}

export default async function ResourceDetailPage({
  params,
}: {
  params: Promise<{ resourceId: string }>;
}) {
  const { resourceId } = await params;
  const orgId = await getActiveOrgId();
  if (!orgId) redirect("/login");

  const detail = await getResourceDetail(resourceId);
  if (!detail || detail.orgId !== orgId) notFound();

  // Managing secrets (create/reveal/delete) is Project Admin+, matching the CP
  // route gates; a Developer sees masked metadata only.
  const { role } = await requireMembership(orgId);
  const canManage = role === "Org Admin" || role === "Project Admin";
  const secrets = await effectiveSecrets(
    orgId,
    detail.resource.projectId,
    detail.resource.environmentId
  );
  const domains = await loadDomains(orgId, resourceId, detail.resource.kind);

  return (
    <ResourceDetail
      detail={{ ...detail, secrets, canManage }}
      orgId={orgId}
      domains={domains}
      domainsEnabled={cpEnabled()}
    />
  );
}
