"use client";

import * as React from "react";
import { toast } from "sonner";
import { UserPlus } from "lucide-react";

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
import type { Role } from "@/lib/mock";

const ROLES: { value: Role; hint: string }[] = [
  { value: "Org Admin", hint: "Full access across the organization" },
  { value: "Project Admin", hint: "Manage assigned projects & environments" },
  { value: "Developer", hint: "Deploy and view assigned resources" },
];

export function InviteMemberDialog() {
  const [open, setOpen] = React.useState(false);
  const [email, setEmail] = React.useState("");
  const [role, setRole] = React.useState<Role>("Developer");

  const emailValid = /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email);

  function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    if (!emailValid) return;
    toast.success(`Invitation sent to ${email}`, {
      description: `Role: ${role}`,
    });
    setOpen(false);
    setEmail("");
    setRole("Developer");
  }

  return (
    <Dialog open={open} onOpenChange={setOpen}>
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
              Send an invitation by email and assign a role. They&apos;ll join
              once they accept.
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
            <Select
              value={role}
              onValueChange={(v) => setRole(v as Role)}
            >
              <SelectTrigger className="w-full">
                <SelectValue>{(value) => value as string}</SelectValue>
              </SelectTrigger>
              <SelectContent>
                {ROLES.map((r) => (
                  <SelectItem key={r.value} value={r.value}>
                    <span className="flex flex-col gap-0.5">
                      <span className="font-medium">{r.value}</span>
                      <span className="text-xs text-muted-foreground">
                        {r.hint}
                      </span>
                    </span>
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <p className="text-xs text-muted-foreground">
              Roles are scoped to the org or a project. Environment-scoped roles
              arrive in a later release.
            </p>
          </div>

          <DialogFooter>
            <DialogClose render={<Button variant="outline" type="button" />}>
              Cancel
            </DialogClose>
            <Button type="submit" disabled={!emailValid}>
              Send invitation
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
