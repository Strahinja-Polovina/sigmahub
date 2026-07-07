import { redirect } from "next/navigation";
import { getActiveOrgId, getSessionUser } from "@/server/active-org";
import { getMembers, getOrg } from "@/server/queries";
import { getAuditLog } from "@/server/audit";
import { SettingsView } from "@/components/dashboard/settings/settings-view";

export default async function SettingsPage() {
  const orgId = await getActiveOrgId();
  if (!orgId) redirect("/login");

  const [org, members, audit, sessionUser] = await Promise.all([
    getOrg(orgId),
    getMembers(orgId),
    getAuditLog(orgId),
    getSessionUser(),
  ]);
  if (!org) redirect("/login");

  const currentUserId = sessionUser.id;
  const currentUserRole =
    members.find((m) => m.id === currentUserId)?.role ?? "Developer";

  return (
    <SettingsView
      org={{ id: org.id, name: org.name, slug: org.slug, plan: org.plan }}
      members={members}
      audit={audit.map((a) => ({
        id: a.id,
        actor: a.actor,
        action: a.action,
        target: a.target,
        createdAt: a.createdAt,
      }))}
      currentUserId={currentUserId}
      currentUserRole={currentUserRole}
    />
  );
}
