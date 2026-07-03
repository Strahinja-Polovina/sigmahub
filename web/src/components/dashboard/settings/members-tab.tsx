"use client";

import * as React from "react";
import { Info } from "lucide-react";

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
import { Avatar, AvatarFallback } from "@/components/ui/avatar";
import { Badge } from "@/components/ui/badge";
import { getMembers } from "@/lib/mock";
import type { Member, Role } from "@/lib/mock";
import { InviteMemberDialog } from "./invite-member-dialog";

const ROLE_VARIANT: Record<Role, React.ComponentProps<typeof Badge>["variant"]> =
  {
    "Org Admin": "default",
    "Project Admin": "secondary",
    Developer: "outline",
  };

function initials(name: string) {
  return name
    .split(/\s+/)
    .slice(0, 2)
    .map((p) => p[0]?.toUpperCase() ?? "")
    .join("");
}

export function MembersTab({ orgId }: { orgId: string }) {
  const members = React.useMemo(() => getMembers(orgId), [orgId]);

  return (
    <div className="flex flex-col gap-4">
      <Card>
        <CardHeader className="flex-row items-start justify-between gap-4 border-b">
          <div className="grid gap-1">
            <CardTitle>Members</CardTitle>
            <CardDescription>
              People with access to this organization.
            </CardDescription>
          </div>
          <InviteMemberDialog />
        </CardHeader>
        <CardContent className="px-0">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="pl-4">Member</TableHead>
                <TableHead>Email</TableHead>
                <TableHead className="pr-4 text-right">Role</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {members.map((m) => (
                <MemberRow key={m.id} member={m} />
              ))}
            </TableBody>
          </Table>
        </CardContent>
      </Card>

      <div className="flex items-start gap-2 rounded-lg border border-border bg-muted/40 px-3 py-2.5 text-xs text-muted-foreground">
        <Info className="mt-0.5 size-3.5 shrink-0" />
        <p>
          <span className="font-medium text-foreground">RBAC (v0.1):</span>{" "}
          roles are scoped at the organization and project level. Fine-grained,
          environment-scoped permissions are deferred to a later release.
        </p>
      </div>
    </div>
  );
}

function MemberRow({ member }: { member: Member }) {
  return (
    <TableRow>
      <TableCell className="pl-4">
        <span className="flex items-center gap-2.5">
          <Avatar size="sm">
            <AvatarFallback>{initials(member.name)}</AvatarFallback>
          </Avatar>
          <span className="font-medium text-foreground">{member.name}</span>
        </span>
      </TableCell>
      <TableCell className="text-muted-foreground">{member.email}</TableCell>
      <TableCell className="pr-4 text-right">
        <Badge variant={ROLE_VARIANT[member.role]}>{member.role}</Badge>
      </TableCell>
    </TableRow>
  );
}
