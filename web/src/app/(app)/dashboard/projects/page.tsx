import { redirect } from "next/navigation";
import {
  getActiveOrgId,
  getMyOrgs,
  getSessionUser,
  visibleProjects,
} from "@/server/active-org";
import { getProjectSummaries } from "@/server/queries";
import { ProjectsView } from "@/components/dashboard/projects/projects-view";

export default async function ProjectsPage() {
  const orgId = await getActiveOrgId();
  if (!orgId) redirect("/login");

  const [myOrgs, sessionUser] = await Promise.all([getMyOrgs(), getSessionUser()]);
  const orgRole = myOrgs.find((o) => o.id === orgId)?.role ?? "Developer";
  const visible = await visibleProjects(sessionUser.id, orgId, orgRole);
  const summaries = await getProjectSummaries(orgId, visible);
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
