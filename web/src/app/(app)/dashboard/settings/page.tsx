import { redirect } from "next/navigation";
import { getActiveOrgId, getSessionUser } from "@/server/active-org";
import { getMembers, getOrg } from "@/server/queries";
import { getAuditLog } from "@/server/audit";
import { cpEnabled, cpListAudit } from "@/server/cp";
import { SettingsView } from "@/components/dashboard/settings/settings-view";

export default async function SettingsPage() {
  const orgId = await getActiveOrgId();
  if (!orgId) redirect("/login");

  const [org, members, audit, sessionUser, cpAudit] = await Promise.all([
    getOrg(orgId),
    getMembers(orgId),
    getAuditLog(orgId),
    getSessionUser(),
    // CP mode: merge the control plane's audit stream (register, tokens,
    // status flips, domain mutations) with the local web audit.
    cpEnabled() ? cpListAudit(orgId).catch(() => []) : Promise.resolve([]),
  ]);
  if (!org) redirect("/login");

  const currentUserId = sessionUser.id;
  const currentUserRole =
    members.find((m) => m.id === currentUserId)?.role ?? "Developer";

  const merged = [
    ...audit.map((a) => ({
      id: a.id,
      actor: a.actor,
      action: a.action,
      target: a.target,
      createdAt: a.createdAt,
    })),
    ...cpAudit.map((a) => ({
      id: `cp_${a.id}`,
      actor: a.actor,
      action: a.action,
      target: a.target,
      createdAt: new Date(a.createdAt),
    })),
  ]
    .sort((x, y) => y.createdAt.getTime() - x.createdAt.getTime())
    .slice(0, 50);

  return (
    <SettingsView
      org={{ id: org.id, name: org.name, slug: org.slug, plan: org.plan }}
      members={members}
      audit={merged}
      currentUserId={currentUserId}
      currentUserRole={currentUserRole}
      cpMode={cpEnabled()}
      orgCreatedAt={org.createdAt}
    />
  );
}
