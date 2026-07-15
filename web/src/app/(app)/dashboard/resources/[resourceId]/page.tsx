import { notFound, redirect } from "next/navigation";
import { getActiveOrgId, requireMembership } from "@/server/active-org";
import { getResourceDetail } from "@/server/queries";
import { effectiveSecrets } from "@/server/secrets-data";
import { ResourceDetail } from "@/components/dashboard/resources/resource-detail";

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

  return <ResourceDetail detail={{ ...detail, secrets, canManage }} />;
}
