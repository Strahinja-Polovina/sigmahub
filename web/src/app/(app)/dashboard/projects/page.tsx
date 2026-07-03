import { redirect } from "next/navigation";
import { getActiveOrgId, getMyOrgs } from "@/server/active-org";
import { getProjectSummaries } from "@/server/queries";
import { ProjectsView } from "@/components/dashboard/projects/projects-view";

export default async function ProjectsPage() {
  const orgId = await getActiveOrgId();
  if (!orgId) redirect("/login");

  const [summaries, myOrgs] = await Promise.all([
    getProjectSummaries(orgId),
    getMyOrgs(),
  ]);
  const orgName = myOrgs.find((o) => o.id === orgId)?.name ?? "your organization";

  const projects = summaries.map((s) => ({
    id: s.project.id,
    name: s.project.name,
    description: s.project.description,
    envCount: s.envCount,
    serverCount: s.serverCount,
    resourceCount: s.resourceCount,
    statusCounts: s.statusCounts,
  }));

  return <ProjectsView orgId={orgId} orgName={orgName} projects={projects} />;
}
