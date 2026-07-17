"use client";

import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { GeneralTab } from "./general-tab";
import { MembersTab } from "./members-tab";
import { AuditTab } from "./audit-tab";
import { TokensTab } from "./tokens-tab";
import { BetaMetricsTab } from "./beta-metrics-tab";
import { SecurityTab } from "./security-tab";
import { AlertsTab } from "./alerts-tab";

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
  audit,
  currentUserId,
  currentUserRole,
  cpMode = false,
  orgCreatedAt = null,
  twoFactorEnabled = false,
}: {
  org: SettingsOrg;
  members: SettingsMember[];
  audit: AuditEntry[];
  currentUserId: string;
  currentUserRole: string;
  /** CP mode adds the beta-metrics tab (P1-13 M1 instrumentation). */
  cpMode?: boolean;
  orgCreatedAt?: string | Date | null;
  twoFactorEnabled?: boolean;
}) {
  const isAdmin = currentUserRole === "Org Admin";

  return (
    <div className="flex flex-col gap-6 p-4 md:p-6">
      <div className="flex flex-col gap-1">
        <h1 className="text-xl font-semibold tracking-tight text-foreground">Settings</h1>
        <p className="text-sm text-muted-foreground">
          Manage {org.name}: general details, members, and the audit log.
        </p>
      </div>

      <Tabs defaultValue="general" className="gap-4">
        <TabsList>
          <TabsTrigger value="general">General</TabsTrigger>
          <TabsTrigger value="members">Members</TabsTrigger>
          <TabsTrigger value="tokens">Tokens</TabsTrigger>
          <TabsTrigger value="security">Security</TabsTrigger>
          <TabsTrigger value="audit">Audit log</TabsTrigger>
          {cpMode && <TabsTrigger value="alerts">Alerts</TabsTrigger>}
          {cpMode && <TabsTrigger value="beta">Beta metrics</TabsTrigger>}
        </TabsList>

        <TabsContent value="general">
          <GeneralTab org={org} isAdmin={isAdmin} />
        </TabsContent>
        <TabsContent value="members">
          <MembersTab
            orgId={org.id}
            members={members}
            currentUserId={currentUserId}
            isAdmin={isAdmin}
          />
        </TabsContent>
        <TabsContent value="tokens">
          <TokensTab orgId={org.id} isAdmin={isAdmin} />
        </TabsContent>
        <TabsContent value="security">
          <SecurityTab initialTwoFactorEnabled={twoFactorEnabled} />
        </TabsContent>
        <TabsContent value="audit">
          <AuditTab entries={audit} />
        </TabsContent>
        {cpMode && (
          <TabsContent value="alerts">
            <AlertsTab orgId={org.id} isAdmin={isAdmin} />
          </TabsContent>
        )}
        {cpMode && (
          <TabsContent value="beta">
            <BetaMetricsTab orgId={org.id} orgCreatedAt={orgCreatedAt} />
          </TabsContent>
        )}
      </Tabs>
    </div>
  );
}
