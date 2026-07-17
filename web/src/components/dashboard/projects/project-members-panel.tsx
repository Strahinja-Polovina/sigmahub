"use client";

import * as React from "react";
import { Users } from "lucide-react";
import { toast } from "sonner";

import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { revokeProjectRole, setProjectRole } from "@/server/actions/project-members";

export type ProjectMemberRow = {
  userId: string;
  name: string;
  email: string;
  orgRole: string;
  /** Their grant on THIS project; null = no grant (org default applies). */
  grantedRole: string | null;
};

const ORG_DEFAULT = "__org__";

/** P2-7 per-project roles. The org role is the ceiling; a grant scopes and
 *  narrows. Members with no grant follow the org default: org-wide access
 *  while they hold zero grants anywhere, invisible here once they hold any. */
export function ProjectMembersPanel({
  orgId,
  projectId,
  members,
  canManage,
}: {
  orgId: string;
  projectId: string;
  members: ProjectMemberRow[];
  canManage: boolean;
}) {
  const [pending, startTransition] = React.useTransition();

  function change(userId: string, value: string) {
    startTransition(async () => {
      try {
        if (value === ORG_DEFAULT) {
          await revokeProjectRole({ orgId, projectId, userId });
          toast.success("Grant removed — org default applies");
        } else {
          await setProjectRole({ orgId, projectId, userId, role: value });
          toast.success(`Granted ${value}`);
        }
      } catch (err) {
        toast.error("Couldn’t update project role", {
          description: err instanceof Error ? err.message : "Please try again.",
        });
      }
    });
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-sm">
          <Users className="size-4 text-muted-foreground" />
          Project members
        </CardTitle>
        <CardDescription>
          The org role is always the ceiling — a grant can scope and narrow access, never
          widen it. Members without any project grant keep org-wide access; their first
          grant scopes them to granted projects only.
        </CardDescription>
      </CardHeader>
      <CardContent>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="pl-0">Member</TableHead>
              <TableHead>Org role</TableHead>
              <TableHead className="pr-0">This project</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {members.map((m) => (
              <TableRow key={m.userId}>
                <TableCell className="pl-0">
                  <div className="flex flex-col">
                    <span className="text-sm font-medium text-foreground">{m.name}</span>
                    <span className="text-xs text-muted-foreground">{m.email}</span>
                  </div>
                </TableCell>
                <TableCell className="text-sm text-muted-foreground">{m.orgRole}</TableCell>
                <TableCell className="pr-0">
                  {m.orgRole === "Org Admin" ? (
                    <span className="text-sm text-muted-foreground">Full access (org admin)</span>
                  ) : canManage ? (
                    <Select
                      value={m.grantedRole ?? ORG_DEFAULT}
                      onValueChange={(v) => change(m.userId, v ?? ORG_DEFAULT)}
                      disabled={pending}
                    >
                      <SelectTrigger className="h-8 w-44">
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value={ORG_DEFAULT}>Org default</SelectItem>
                        <SelectItem value="Project Admin">Project Admin</SelectItem>
                        <SelectItem value="Developer">Developer</SelectItem>
                      </SelectContent>
                    </Select>
                  ) : (
                    <span className="text-sm text-muted-foreground">
                      {m.grantedRole ?? "Org default"}
                    </span>
                  )}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </CardContent>
    </Card>
  );
}
