import { redirect } from "next/navigation";
import { getActiveOrgId } from "@/server/active-org";
import { getProjectsWithEnvs } from "@/server/queries";
import { SidebarInset, SidebarProvider } from "@/components/ui/sidebar";
import { OrgProvider } from "@/components/dashboard/org-context";
import { AppSidebar } from "@/components/dashboard/app-sidebar";
import { TopBar } from "@/components/dashboard/top-bar";

export default async function AppLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  // Gate the whole dashboard on a valid session (getActiveOrgId → getSessionUser
  // redirects to /login when unauthenticated) and resolve the active org.
  const orgId = await getActiveOrgId();
  if (!orgId) redirect("/login");
  const projects = await getProjectsWithEnvs(orgId);

  return (
    <OrgProvider initialOrgId={orgId}>
      <SidebarProvider>
        <AppSidebar projects={projects} />
        <SidebarInset>
          <TopBar />
          <div className="flex flex-1 flex-col">{children}</div>
        </SidebarInset>
      </SidebarProvider>
    </OrgProvider>
  );
}
