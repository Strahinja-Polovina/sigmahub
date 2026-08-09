"use client";

import { useSearchParams } from "next/navigation";

import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { GeneralTab } from "./general-tab";
import { MembersTab } from "./members-tab";
import { AuditTab } from "./audit-tab";
import { TokensTab } from "./tokens-tab";
import { BetaMetricsTab } from "./beta-metrics-tab";
import { ControlPlaneNote } from "@/components/dashboard/control-plane-note";
import { SecurityTab } from "./security-tab";
import { AlertsTab } from "./alerts-tab";
import { IntegrationsTab, type Installation } from "./integrations-tab";
import type { CpImageRegistry } from "@/server/cp";

export type SettingsOrg = {
  id: string;
  name: string;
  slug: string;
  plan: string;
};
export type SettingsMember = {
  id: string;
  name: string;
  email: string;
  role: string;
  /** SIGMA-167: explicitly project-scoped (sees only granted projects). */
  scoped: boolean;
};
export type PendingInvite = {
  id: string;
  email: string;
  role: string;
  invitedBy: string;
  expiresAt: string | Date;
};
export type AuditEntry = {
  id: string;
  actor: string;
  action: string;
  target: string;
  createdAt: string | Date;
};

export function SettingsView({
  org,
  members,
  pendingInvites = [],
  audit,
  canViewAudit = true,
  currentUserId,
  currentUserRole,
  cpMode = false,
  orgCreatedAt = null,
  twoFactorEnabled = false,
  gitIntegration = null,
  registry,
}: {
  org: SettingsOrg;
  members: SettingsMember[];
  pendingInvites?: PendingInvite[];
  audit: AuditEntry[];
  /** The org-wide audit exposes cross-project resource ids, so it's shown only
   *  to users who can see every project in the org (SIGMA-97). */
  canViewAudit?: boolean;
  currentUserId: string;
  currentUserRole: string;
  /** CP mode adds the beta-metrics tab (P1-13 M1 instrumentation). */
  cpMode?: boolean;
  orgCreatedAt?: string | Date | null;
  twoFactorEnabled?: boolean;
  /** Org-level GitHub integration (CP mode); null when unavailable. */
  gitIntegration?: {
    enabled: boolean;
    slug: string;
    installations: Installation[];
  } | null;
  /** The org's container registry, shown alongside GitHub in Integrations. */
  registry?: {
    configured: boolean;
    registry: CpImageRegistry | null;
    repository: string;
  };
}) {
  const isAdmin = currentUserRole === "Org Admin";

  // Deep-linkable tabs: the org switcher (and other surfaces) link to
  // /dashboard/settings?tab=members|audit|..., so honor a valid ?tab param as
  // the initial tab and fall back to "general" for anything unknown/hidden.
  const searchParams = useSearchParams();
  const requestedTab = searchParams.get("tab");
  const availableTabs = [
    "general",
    "members",
    "tokens",
    "security",
    ...(canViewAudit ? ["audit"] : []),
    // Present in both modes. Hiding these three with no control plane meant
    // someone evaluating SigmaHub offline concluded it has no GitHub App, no
    // registry, no alerting and no beta feed — rather than that they cannot
    // configure those from here. Each one now explains itself instead
    // (SIGMA-215).
    "integrations",
    "alerts",
    "beta",
  ];
  const initialTab =
    requestedTab && availableTabs.includes(requestedTab) ? requestedTab : "general";

  return (
    <div className="flex flex-col gap-6 p-4 md:p-6">
      <div className="flex flex-col gap-1">
        <h1 className="text-xl font-semibold tracking-tight text-foreground">Settings</h1>
        <p className="text-sm text-muted-foreground">
          Manage {org.name}: general details, members, and the audit log.
        </p>
      </div>

      <Tabs defaultValue={initialTab} className="gap-4">
        <TabsList>
          <TabsTrigger value="general">General</TabsTrigger>
          <TabsTrigger value="members">Members</TabsTrigger>
          <TabsTrigger value="tokens">Tokens</TabsTrigger>
          <TabsTrigger value="integrations">Integrations</TabsTrigger>
          <TabsTrigger value="security">Security</TabsTrigger>
          {canViewAudit && <TabsTrigger value="audit">Audit log</TabsTrigger>}
          <TabsTrigger value="alerts">Alerts</TabsTrigger>
          <TabsTrigger value="beta">Beta metrics</TabsTrigger>
        </TabsList>

        <TabsContent value="general">
          <GeneralTab org={org} isAdmin={isAdmin} />
        </TabsContent>
        <TabsContent value="members">
          <MembersTab
            orgId={org.id}
            members={members}
            pendingInvites={pendingInvites}
            currentUserId={currentUserId}
            isAdmin={isAdmin}
          />
        </TabsContent>
        <TabsContent value="tokens">
          <TokensTab orgId={org.id} isAdmin={isAdmin} />
        </TabsContent>
        <TabsContent value="integrations">
          {cpMode ? (
            <IntegrationsTab
              orgId={org.id}
              enabled={gitIntegration?.enabled ?? false}
              slug={gitIntegration?.slug ?? ""}
              installations={gitIntegration?.installations ?? []}
              canManage={isAdmin}
              registry={registry}
            />
          ) : (
            <ControlPlaneNote title="The GitHub App and the image registry live on the control plane">
              Two org-wide integrations are configured here. The GitHub App is installed
              once and gives every project a repository picker and push-to-deploy; the
              container registry is what makes an image built on one of your machines
              runnable on all of them. Both hold credentials the control plane owns — an
              installation token and a registry password — so neither can be set up
              without one.
            </ControlPlaneNote>
          )}
        </TabsContent>
        <TabsContent value="security">
          <SecurityTab initialTwoFactorEnabled={twoFactorEnabled} />
        </TabsContent>
        {canViewAudit && (
          <TabsContent value="audit">
            <AuditTab entries={audit} />
          </TabsContent>
        )}
        <TabsContent value="alerts">
          {cpMode ? (
            <AlertsTab orgId={org.id} isAdmin={isAdmin} />
          ) : (
            <ControlPlaneNote title="Alerts are delivered by the control plane">
              With a control plane, you add Slack, email or webhook channels, choose which
              operational events each one receives — a server going unreachable, a deploy
              failing, a backup that did not verify — and fire a real test notification to
              prove delivery works before you rely on it. Delivery is the control plane
              making an outbound call when something happens to your fleet, and there is
              no fleet being watched here.
            </ControlPlaneNote>
          )}
        </TabsContent>
        <TabsContent value="beta">
          {cpMode ? (
            <BetaMetricsTab orgId={org.id} orgCreatedAt={orgCreatedAt} />
          ) : (
            <ControlPlaneNote title="Beta metrics are measured on the control plane">
              This tab reports how this organization is doing against the beta&apos;s exit
              criteria — time from connecting a server to a first successful deploy,
              deploy success rate, agent version spread. Every figure is measured from
              real events the control plane recorded, so with none recorded there is
              nothing to report rather than a chart of nothing.
            </ControlPlaneNote>
          )}
        </TabsContent>
      </Tabs>
    </div>
  );
}
