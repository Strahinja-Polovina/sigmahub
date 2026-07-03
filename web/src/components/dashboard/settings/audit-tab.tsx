"use client";

import * as React from "react";

import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Badge } from "@/components/ui/badge";
import { getMembers } from "@/lib/mock";

type AuditEntry = {
  actor: string;
  action: string;
  category: "deploy" | "server" | "access" | "billing" | "settings";
  target: string;
  at: string; // relative time label
};

// Deterministic, org-scoped mock audit trail (no backend). Actors are drawn
// from the org's members so the log tracks the active organization.
function buildAuditLog(orgId: string): AuditEntry[] {
  const members = getMembers(orgId);
  const actor = (i: number) => members[i % members.length]?.name ?? "system";

  const template: Omit<AuditEntry, "actor">[] = [
    { action: "Deployed resource", category: "deploy", target: "api-gateway · v452", at: "2 min ago" },
    { action: "Connected server", category: "server", target: "ash-general-03", at: "1 hr ago" },
    { action: "Invited member", category: "access", target: "teammate@company.com · Developer", at: "3 hr ago" },
    { action: "Restarted resource", category: "deploy", target: "worker", at: "5 hr ago" },
    { action: "Downloaded invoice", category: "billing", target: "Period Jun 2026", at: "Yesterday" },
    { action: "Changed role", category: "access", target: "Nikola Petrović → Developer", at: "Yesterday" },
    { action: "Updated org name", category: "settings", target: "Organization settings", at: "2 days ago" },
    { action: "Rotated agent token", category: "server", target: "hel-db-01", at: "3 days ago" },
    { action: "Deployed resource", category: "deploy", target: "storefront · v128", at: "4 days ago" },
    { action: "Removed member", category: "access", target: "contractor@ext.com", at: "5 days ago" },
  ];

  return template.map((t, i) => ({ actor: actor(i), ...t }));
}

const CATEGORY_VARIANT: Record<
  AuditEntry["category"],
  React.ComponentProps<typeof Badge>["variant"]
> = {
  deploy: "secondary",
  server: "outline",
  access: "outline",
  billing: "outline",
  settings: "outline",
};

const CATEGORY_LABEL: Record<AuditEntry["category"], string> = {
  deploy: "Deploy",
  server: "Server",
  access: "Access",
  billing: "Billing",
  settings: "Settings",
};

export function AuditTab({ orgId }: { orgId: string }) {
  const entries = React.useMemo(() => buildAuditLog(orgId), [orgId]);

  return (
    <Card>
      <CardHeader className="border-b">
        <CardTitle>Audit log</CardTitle>
        <CardDescription>
          Recent audited actions across this organization.
        </CardDescription>
      </CardHeader>
      <CardContent className="px-0">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="pl-4">Actor</TableHead>
              <TableHead>Action</TableHead>
              <TableHead>Target</TableHead>
              <TableHead className="pr-4 text-right">When</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {entries.map((e, i) => (
              <TableRow key={i}>
                <TableCell className="pl-4 font-medium text-foreground">
                  {e.actor}
                </TableCell>
                <TableCell>
                  <span className="flex items-center gap-2">
                    <Badge variant={CATEGORY_VARIANT[e.category]}>
                      {CATEGORY_LABEL[e.category]}
                    </Badge>
                    <span className="text-foreground">{e.action}</span>
                  </span>
                </TableCell>
                <TableCell className="font-mono text-xs text-muted-foreground">
                  {e.target}
                </TableCell>
                <TableCell className="pr-4 text-right text-muted-foreground tabular-nums">
                  {e.at}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </CardContent>
    </Card>
  );
}
