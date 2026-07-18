"use client";

import * as React from "react";
import { toast } from "sonner";
import { Loader2, UserPlus } from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { inviteMember } from "@/server/actions/members";

const ROLES: { value: string; hint: string }[] = [
  { value: "Org Admin", hint: "Full access across the organization" },
  { value: "Project Admin", hint: "Manage assigned projects & environments" },
  { value: "Developer", hint: "Deploy and view assigned resources" },
];

export function InviteMemberDialog({ orgId }: { orgId: string }) {
  const [open, setOpen] = React.useState(false);
  const [email, setEmail] = React.useState("");
  const [role, setRole] = React.useState("Developer");
  const [pending, startTransition] = React.useTransition();

  const emailValid = /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email);

  function reset() {
    setEmail("");
    setRole("Developer");
  }

  function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    if (!emailValid) return;
    startTransition(async () => {
      try {
        const { delivered, inviteUrl } = await inviteMember({ orgId, email, role });
        if (delivered) {
          toast.success(`Invitation sent to ${email}`, { description: `Role: ${role}` });
        } else {
          // Honest degradation: no mail transport is wired, so copy the link
          // for the admin to relay rather than pretend an email went out.
          await navigator.clipboard?.writeText(inviteUrl).catch(() => {});
          toast.success(`Invite created for ${email}`, {
            description: "Email delivery isn’t configured — the invite link was copied to your clipboard.",
          });
        }
        setOpen(false);
        reset();
      } catch (err) {
        toast.error("Couldn’t create invite", {
          description: err instanceof Error ? err.message : "Please try again.",
        });
      }
    });
  }

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (pending) return;
        setOpen(next);
        if (!next) reset();
      }}
    >
      <DialogTrigger
        render={
          <Button size="sm" className="gap-1.5">
            <UserPlus className="size-3.5" />
            Invite member
          </Button>
        }
      />
      <DialogContent>
        <form onSubmit={handleSubmit} className="flex flex-col gap-4">
          <DialogHeader>
            <DialogTitle>Invite a member</DialogTitle>
            <DialogDescription>
              Send a teammate an invite link by email. They join with the assigned role once
              they accept and sign in.
            </DialogDescription>
          </DialogHeader>

          <div className="flex flex-col gap-2">
            <Label htmlFor="invite-email">Email address</Label>
            <Input
              id="invite-email"
              type="email"
              placeholder="teammate@company.com"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              autoComplete="off"
              required
            />
          </div>

          <div className="flex flex-col gap-2">
            <Label>Role</Label>
            <Select value={role} onValueChange={(v) => setRole(v as string)}>
              <SelectTrigger className="w-full">
                <SelectValue>{(value) => value as string}</SelectValue>
              </SelectTrigger>
              <SelectContent>
                {ROLES.map((r) => (
                  <SelectItem key={r.value} value={r.value}>
                    <span className="flex flex-col gap-0.5">
                      <span className="font-medium">{r.value}</span>
                      <span className="text-xs text-muted-foreground">{r.hint}</span>
                    </span>
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <p className="text-xs text-muted-foreground">
              Roles are scoped to the org or a project. Environment-scoped roles arrive in a later
              release.
            </p>
          </div>

          <DialogFooter>
            <DialogClose render={<Button variant="outline" type="button" disabled={pending} />}>
              Cancel
            </DialogClose>
            <Button type="submit" disabled={!emailValid || pending}>
              {pending && <Loader2 className="size-4 animate-spin" />}
              Send invite
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
