import { redirect } from "next/navigation";
import { getActiveOrgId, getMyOrgs, requireMembership, visibleProjects } from "@/server/active-org";
import { getDeployTargets, getOrgResources } from "@/server/queries";
import { ResourcesView } from "@/components/dashboard/resources/resources-view";
import { cpEnabled } from "@/server/cp";
import { listClusters } from "@/server/actions/clusters";
import { WIZARD_RESUME_PARAM, WIZARD_RESUME_VALUE } from "@/lib/wizard/resume";

export default async function ResourcesPage({
  searchParams,
}: {
  searchParams: Promise<Record<string, string | string[] | undefined>>;
}) {
  const orgId = await getActiveOrgId();
  if (!orgId) redirect("/login");

  // P2-7 read scoping: only list resources — and offer deploy targets — in
  // projects the user was granted (SIGMA-75).
  const { user, role } = await requireMembership(orgId);
  const visible = await visibleProjects(user.id, orgId, role);

  const [resources, targets, myOrgs, clusterData, params] = await Promise.all([
    getOrgResources(orgId, visible),
    getDeployTargets(orgId, visible),
    getMyOrgs(),
    // Clusters are a deploy TARGET, and the wizard could not offer them because
    // nothing loaded them here — clusterId has threaded end to end since
    // SIGMA-200 with no control able to set it.
    cpEnabled() ? listClusters(orgId) : Promise.resolve({ clusters: [], excludedKinds: [] }),
    searchParams,
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

  return (
    <ResourcesView
      orgName={orgName}
      resources={items}
      targets={targets}
      cpMode={cpEnabled()}
      orgId={orgId}
      clusters={clusterData.clusters}
      clusterExcludedKinds={clusterData.excludedKinds}
      // Returning from the GitHub App install: reopen the wizard where it was.
      resumeWizard={params[WIZARD_RESUME_PARAM] === WIZARD_RESUME_VALUE}
    />
  );
}
