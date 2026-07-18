"use client";

import * as React from "react";
import { toast } from "sonner";
import { Info, Loader2, MoreHorizontal, Trash2, UserCog } from "lucide-react";

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
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Avatar, AvatarFallback } from "@/components/ui/avatar";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { changeMemberRole, removeMember, resendInvite, revokeInvite } from "@/server/actions/members";
import type { PendingInvite, SettingsMember } from "./settings-view";
import { InviteMemberDialog } from "./invite-member-dialog";
import { Mail, RefreshCw, X } from "lucide-react";

const ROLES = ["Org Admin", "Project Admin", "Developer"] as const;
const ROLE_VARIANT: Record<string, React.ComponentProps<typeof Badge>["variant"]> = {
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

const msg = (err: unknown) => (err instanceof Error ? err.message : "Please try again.");

function MemberActions({
  orgId,
  member,
  isSelf,
}: {
  orgId: string;
  member: SettingsMember;
  isSelf: boolean;
}) {
  const [pending, startTransition] = React.useTransition();

  function setRole(role: string) {
    if (role === member.role) return;
    startTransition(async () => {
      try {
        await changeMemberRole({ orgId, userId: member.id, role });
        toast.success(`${member.name} is now ${role}`);
      } catch (err) {
        toast.error("Couldn’t change role", { description: msg(err) });
      }
    });
  }

  function remove() {
    startTransition(async () => {
      try {
        await removeMember({ orgId, userId: member.id });
        toast.success(`${member.name} removed`);
      } catch (err) {
        toast.error("Couldn’t remove member", { description: msg(err) });
      }
    });
  }

  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        render={
          <Button variant="ghost" size="icon-sm" aria-label={`Manage ${member.name}`} disabled={pending}>
            {pending ? <Loader2 className="size-4 animate-spin" /> : <MoreHorizontal className="size-4" />}
          </Button>
        }
      />
      <DropdownMenuContent align="end" className="w-48">
        <DropdownMenuLabel className="flex items-center gap-2 text-xs">
          <UserCog className="size-3.5 text-muted-foreground" />
          Change role
        </DropdownMenuLabel>
        {ROLES.map((r) => (
          <DropdownMenuItem
            key={r}
            className="gap-2"
            disabled={r === member.role}
            onClick={() => setRole(r)}
          >
            {r}
            {r === member.role && <span className="ml-auto text-xs text-muted-foreground">current</span>}
          </DropdownMenuItem>
        ))}
        <DropdownMenuSeparator />
        <DropdownMenuItem
          variant="destructive"
          className="gap-2"
          disabled={isSelf}
          onClick={remove}
        >
          <Trash2 className="size-4" />
          {isSelf ? "Can’t remove yourself" : "Remove from org"}
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

function PendingInviteRow({ orgId, invite }: { orgId: string; invite: PendingInvite }) {
  const [pending, startTransition] = React.useTransition();
  const expires = new Date(invite.expiresAt);

  function resend() {
    startTransition(async () => {
      try {
        const { delivered, inviteUrl } = await resendInvite({ orgId, invitationId: invite.id });
        if (delivered) {
          toast.success(`Invite re-sent to ${invite.email}`);
        } else {
          await navigator.clipboard?.writeText(inviteUrl).catch(() => {});
          toast.success("Invite link refreshed", {
            description: "Email delivery isn’t configured — the new link was copied to your clipboard.",
          });
        }
      } catch (err) {
        toast.error("Couldn’t resend invite", { description: msg(err) });
      }
    });
  }

  function revoke() {
    startTransition(async () => {
      try {
        await revokeInvite({ orgId, invitationId: invite.id });
        toast.success(`Invite to ${invite.email} revoked`);
      } catch (err) {
        toast.error("Couldn’t revoke invite", { description: msg(err) });
      }
    });
  }

  return (
    <TableRow>
      <TableCell className="pl-4">
        <span className="flex items-center gap-2.5">
          <span className="grid size-8 place-items-center rounded-full bg-muted text-muted-foreground">
            <Mail className="size-4" />
          </span>
          <span className="font-medium text-foreground">{invite.email}</span>
        </span>
      </TableCell>
      <TableCell className="text-muted-foreground">
        Invited by {invite.invitedBy} · expires {expires.toLocaleDateString()}
      </TableCell>
      <TableCell>
        <Badge variant={ROLE_VARIANT[invite.role] ?? "outline"}>{invite.role}</Badge>
      </TableCell>
      <TableCell className="pr-4 text-right">
        <span className="inline-flex items-center gap-1">
          <Button
            variant="ghost"
            size="icon-sm"
            aria-label={`Resend invite to ${invite.email}`}
            disabled={pending}
            onClick={resend}
          >
            {pending ? <Loader2 className="size-4 animate-spin" /> : <RefreshCw className="size-4" />}
          </Button>
          <Button
            variant="ghost"
            size="icon-sm"
            aria-label={`Revoke invite to ${invite.email}`}
            disabled={pending}
            onClick={revoke}
          >
            <X className="size-4 text-muted-foreground" />
          </Button>
        </span>
      </TableCell>
    </TableRow>
  );
}

export function MembersTab({
  orgId,
  members,
  pendingInvites,
  currentUserId,
  isAdmin,
}: {
  orgId: string;
  members: SettingsMember[];
  pendingInvites: PendingInvite[];
  currentUserId: string;
  isAdmin: boolean;
}) {
  return (
    <div className="flex flex-col gap-4">
      <Card>
        <CardHeader className="flex-row items-start justify-between gap-4 border-b">
          <div className="grid gap-1">
            <CardTitle>Members</CardTitle>
            <CardDescription>People with access to this organization.</CardDescription>
          </div>
          {isAdmin && <InviteMemberDialog orgId={orgId} />}
        </CardHeader>
        <CardContent className="px-0">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="pl-4">Member</TableHead>
                <TableHead>Email</TableHead>
                <TableHead className={isAdmin ? "" : "pr-4 text-right"}>Role</TableHead>
                {isAdmin && (
                  <TableHead className="w-10 pr-4 text-right">
                    <span className="sr-only">Actions</span>
                  </TableHead>
                )}
              </TableRow>
            </TableHeader>
            <TableBody>
              {members.map((m) => (
                <TableRow key={m.id}>
                  <TableCell className="pl-4">
                    <span className="flex items-center gap-2.5">
                      <Avatar size="sm">
                        <AvatarFallback>{initials(m.name)}</AvatarFallback>
                      </Avatar>
                      <span className="font-medium text-foreground">
                        {m.name}
                        {m.id === currentUserId && (
                          <span className="ml-1.5 text-xs text-muted-foreground">(you)</span>
                        )}
                      </span>
                    </span>
                  </TableCell>
                  <TableCell className="text-muted-foreground">{m.email}</TableCell>
                  <TableCell className={isAdmin ? "" : "pr-4 text-right"}>
                    <Badge variant={ROLE_VARIANT[m.role] ?? "outline"}>{m.role}</Badge>
                  </TableCell>
                  {isAdmin && (
                    <TableCell className="pr-4 text-right">
                      <MemberActions orgId={orgId} member={m} isSelf={m.id === currentUserId} />
                    </TableCell>
                  )}
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </CardContent>
      </Card>

      {isAdmin && pendingInvites.length > 0 && (
        <Card>
          <CardHeader className="border-b">
            <CardTitle className="text-base">Pending invitations</CardTitle>
            <CardDescription>
              These people have been invited but haven’t accepted yet.
            </CardDescription>
          </CardHeader>
          <CardContent className="px-0">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead className="pl-4">Email</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Role</TableHead>
                  <TableHead className="w-20 pr-4 text-right">
                    <span className="sr-only">Actions</span>
                  </TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {pendingInvites.map((inv) => (
                  <PendingInviteRow key={inv.id} orgId={orgId} invite={inv} />
                ))}
              </TableBody>
            </Table>
          </CardContent>
        </Card>
      )}

      <div className="flex items-start gap-2 rounded-lg border border-border bg-muted/40 px-3 py-2.5 text-xs text-muted-foreground">
        <Info className="mt-0.5 size-3.5 shrink-0" />
        <p>
          <span className="font-medium text-foreground">RBAC (v0.1):</span> roles are scoped at the
          organization and project level. Fine-grained, environment-scoped permissions are deferred
          to a later release.
        </p>
      </div>
    </div>
  );
}
