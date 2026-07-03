import { redirect } from "next/navigation";
import { getActiveOrgId, getMyOrgs } from "@/server/active-org";
import { getDeployTargets, getOrgResources } from "@/server/queries";
import { ResourcesView } from "@/components/dashboard/resources/resources-view";

export default async function ResourcesPage() {
  const orgId = await getActiveOrgId();
  if (!orgId) redirect("/login");

  const [resources, targets, myOrgs] = await Promise.all([
    getOrgResources(orgId),
    getDeployTargets(orgId),
    getMyOrgs(),
  ]);
  const orgName = myOrgs.find((o) => o.id === orgId)?.name ?? "your organization";

  const items = resources.map((r) => ({
    id: r.id,
    name: r.name,
    kind: r.kind,
    status: r.status,
    projectName: r.projectName,
    envName: r.envName,
    environmentId: r.environmentId,
    lastDeployAt: r.lastDeployAt,
    domain: r.domain,
  }));

  return <ResourcesView orgName={orgName} resources={items} targets={targets} />;
}
