"use client";

import * as React from "react";

import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { useActiveOrg } from "@/components/dashboard/org-context";
import { GeneralTab } from "./general-tab";
import { MembersTab } from "./members-tab";
import { AuditTab } from "./audit-tab";

export function SettingsView() {
  const { orgId, org } = useActiveOrg();

  return (
    <div className="flex flex-col gap-6 p-4 md:p-6">
      <div className="flex flex-col gap-1">
        <h1 className="text-xl font-semibold tracking-tight text-foreground">
          Settings
        </h1>
        <p className="text-sm text-muted-foreground">
          Manage {org.name}: general details, members, and the audit log.
        </p>
      </div>

      <Tabs defaultValue="general" className="gap-4">
        <TabsList>
          <TabsTrigger value="general">General</TabsTrigger>
          <TabsTrigger value="members">Members</TabsTrigger>
          <TabsTrigger value="audit">Audit log</TabsTrigger>
        </TabsList>

        <TabsContent value="general">
          <GeneralTab org={org} />
        </TabsContent>
        <TabsContent value="members">
          <MembersTab orgId={orgId} />
        </TabsContent>
        <TabsContent value="audit">
          <AuditTab orgId={orgId} />
        </TabsContent>
      </Tabs>
    </div>
  );
}
