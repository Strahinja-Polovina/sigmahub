import { redirect } from "next/navigation";
import {
  ensurePersonalOrg,
  getActiveOrgId,
  getMyOrgs,
  getSessionUser,
  visibleProjects,
} from "@/server/active-org";
import {
  getProjectsWithEnvs,
  getServerCounts,
  getCommandIndex,
} from "@/server/queries";
import { maybeSyncOrgMirror } from "@/server/cp-sync";
import { SidebarInset, SidebarProvider } from "@/components/ui/sidebar";
import { OrgProvider } from "@/components/dashboard/org-context";
import { AppSidebar } from "@/components/dashboard/app-sidebar";
import { CpStatusBanner } from "@/components/dashboard/cp-status-banner";
import { TopBar } from "@/components/dashboard/top-bar";

export default async function AppLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  // Gate the whole dashboard on a valid session (getActiveOrgId → getSessionUser
  // redirects to /login when unauthenticated) and resolve the active org.
  let orgId = await getActiveOrgId();
  if (!orgId) {
    // Fresh signup: the user is authenticated but belongs to no org yet.
    await ensurePersonalOrg();
    orgId = await getActiveOrgId();
  }
  if (!orgId) redirect("/login");

  // Repair CP↔mirror drift before the reads below (throttled, no-op in demo
  // mode); the returned health drives the "control plane unreachable" banner.
  const cpStatus = await maybeSyncOrgMirror(orgId);

  const [myOrgs, sessionUser] = await Promise.all([getMyOrgs(), getSessionUser()]);
  // P2-7 project scoping: a user with project grants sees only those projects
  // in the sidebar and ⌘K index; null = org-wide (admins, ungranted users).
  const orgRole = myOrgs.find((o) => o.id === orgId)?.role ?? "Developer";
  const visible = await visibleProjects(sessionUser.id, orgId, orgRole);
  const [projects, commandIndex] = await Promise.all([
    getProjectsWithEnvs(orgId, visible),
    getCommandIndex(orgId, visible),
  ]);
  const counts = await getServerCounts(myOrgs.map((o) => o.id));
  const orgs = myOrgs.map((o) => ({
    id: o.id,
    name: o.name,
    slug: o.slug,
    plan: o.plan,
    serverCount: counts[o.id] ?? 0,
  }));
  const user = {
    id: sessionUser.id,
    name: sessionUser.name,
    email: sessionUser.email,
  };

  return (
    <OrgProvider orgs={orgs} activeOrgId={orgId} user={user}>
      <SidebarProvider>
        <AppSidebar projects={projects} />
        <SidebarInset>
          <TopBar commandIndex={commandIndex} />
          <CpStatusBanner status={cpStatus} />
          <div className="flex flex-1 flex-col">{children}</div>
        </SidebarInset>
      </SidebarProvider>
    </OrgProvider>
  );
}
