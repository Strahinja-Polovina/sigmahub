import { notFound, redirect } from "next/navigation";
import { getActiveOrgId } from "@/server/active-org";
import { getResourceDetail } from "@/server/queries";
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

  return <ResourceDetail detail={detail} />;
}
