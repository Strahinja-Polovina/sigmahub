import { redirect } from "next/navigation";
import { getActiveOrgId } from "@/server/active-org";
import { getEnvironmentPanels, getProject } from "@/server/queries";
import { ProjectDetailView } from "@/components/dashboard/projects/project-detail-view";

export default async function ProjectDetailPage({
  params,
}: {
  params: Promise<{ projectId: string }>;
}) {
  const { projectId } = await params;
  const orgId = await getActiveOrgId();
  if (!orgId) redirect("/login");

  const project = await getProject(projectId);
  // Guard: missing project, or one that belongs to a different org.
  if (!project || project.orgId !== orgId) {
    return <ProjectDetailView project={null} panels={[]} />;
  }

  const panels = await getEnvironmentPanels(projectId);
  return <ProjectDetailView project={project} panels={panels} />;
}
