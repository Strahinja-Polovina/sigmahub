"use client";

import * as React from "react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { toast } from "sonner";
import {
  ChevronsUpDown,
  Check,
  CreditCard,
  Users,
  ScrollText,
  Loader2,
  Plus,
} from "lucide-react";

import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
} from "@/components/ui/sidebar";
import { useActiveOrg } from "@/components/dashboard/org-context";
import { createOrg, setActiveOrg } from "@/server/actions/org";
import { unwrap } from "@/lib/action-result";

function orgInitial(name: string) {
  return name.charAt(0).toUpperCase();
}

export function OrgSwitcher() {
  const router = useRouter();
  const [, startTransition] = React.useTransition();
  const { org, orgs, setOrgId } = useActiveOrg();
  // SIGMA-306: "New organization" used to be a link to /dashboard/settings —
  // i.e. to the CURRENT org's rename form. Creating a second org is its own
  // action, so it gets its own dialog. It lives beside the menu rather than
  // inside it: the dropdown unmounts its content when an item is chosen, and a
  // dialog nested there would close with it.
  const [createOpen, setCreateOpen] = React.useState(false);
  const [newName, setNewName] = React.useState("");
  const [creating, startCreate] = React.useTransition();

  function switchOrg(id: string) {
    if (id === org.id) return;
    setOrgId(id); // immediate client feedback
    startTransition(async () => {
      unwrap(await setActiveOrg(id)); // persist cookie so server components follow
      router.refresh();
    });
  }

  function handleCreate(e: React.FormEvent) {
    e.preventDefault();
    const name = newName.trim();
    if (!name) return;
    startCreate(async () => {
      try {
        unwrap(await createOrg({ name }));
        setCreateOpen(false);
        setNewName("");
        toast.success(`Created ${name}`, {
          description: "You're now in the new organization as its admin.",
        });
        // The action already switched the sh_org cookie; refreshing re-renders
        // the layout (and this switcher) inside the new org.
        router.refresh();
      } catch (err) {
        toast.error("Couldn’t create the organization", {
          description: err instanceof Error ? err.message : "Please try again.",
        });
      }
    });
  }

  return (
    <SidebarMenu>
      <SidebarMenuItem>
        <DropdownMenu>
          <DropdownMenuTrigger
            render={
              <SidebarMenuButton
                size="lg"
                className="data-open:bg-sidebar-accent data-open:text-sidebar-accent-foreground"
              >
                <span className="grid size-8 shrink-0 place-items-center rounded-md bg-primary font-mono text-sm font-semibold text-primary-foreground">
                  {orgInitial(org.name)}
                </span>
                <span className="grid flex-1 text-left leading-tight">
                  <span className="truncate text-sm font-medium">{org.name}</span>
                  <span className="truncate text-xs text-muted-foreground capitalize">
                    {org.plan} plan · {org.serverCount} servers
                  </span>
                </span>
                <ChevronsUpDown className="ml-auto size-4 text-muted-foreground" />
              </SidebarMenuButton>
            }
          />
          <DropdownMenuContent
            className="w-(--anchor-width) min-w-64"
            align="start"
            side="bottom"
            sideOffset={4}
          >
            <DropdownMenuLabel>Organizations</DropdownMenuLabel>
            <DropdownMenuGroup>
              {orgs.map((o) => (
                <DropdownMenuItem
                  key={o.id}
                  onClick={() => switchOrg(o.id)}
                  className="gap-2"
                >
                  <span className="grid size-6 shrink-0 place-items-center rounded bg-muted font-mono text-xs font-semibold text-foreground">
                    {orgInitial(o.name)}
                  </span>
                  <span className="flex-1 truncate">{o.name}</span>
                  {o.id === org.id && <Check className="size-4 text-primary" />}
                </DropdownMenuItem>
              ))}
            </DropdownMenuGroup>
            <DropdownMenuSeparator />
            <DropdownMenuGroup>
              <DropdownMenuItem
                render={<Link href="/dashboard/billing" />}
                className="gap-2"
              >
                <CreditCard className="size-4 text-muted-foreground" />
                Billing
              </DropdownMenuItem>
              <DropdownMenuItem
                render={<Link href="/dashboard/settings?tab=members" />}
                className="gap-2"
              >
                <Users className="size-4 text-muted-foreground" />
                Members
              </DropdownMenuItem>
              <DropdownMenuItem
                render={<Link href="/dashboard/settings?tab=audit" />}
                className="gap-2"
              >
                <ScrollText className="size-4 text-muted-foreground" />
                Audit log
              </DropdownMenuItem>
            </DropdownMenuGroup>
            <DropdownMenuSeparator />
            <DropdownMenuItem onClick={() => setCreateOpen(true)} className="gap-2">
              <Plus className="size-4 text-muted-foreground" />
              New organization
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>

        <Dialog
          open={createOpen}
          onOpenChange={(next) => {
            if (creating) return;
            setCreateOpen(next);
            if (!next) setNewName("");
          }}
        >
          <DialogContent>
            <form onSubmit={handleCreate} className="flex flex-col gap-4">
              <DialogHeader>
                <DialogTitle>New organization</DialogTitle>
                <DialogDescription>
                  A separate space for its own projects, servers and billing. You become
                  its admin and can invite members from Settings.
                </DialogDescription>
              </DialogHeader>

              <div className="flex flex-col gap-2">
                <Label htmlFor="new-org-name">Organization name</Label>
                <Input
                  id="new-org-name"
                  value={newName}
                  onChange={(e) => setNewName(e.target.value)}
                  placeholder="Beta Client"
                  autoComplete="off"
                  maxLength={64}
                  required
                />
              </div>

              <DialogFooter>
                <Button
                  type="button"
                  variant="outline"
                  disabled={creating}
                  onClick={() => setCreateOpen(false)}
                >
                  Cancel
                </Button>
                <Button type="submit" disabled={creating || !newName.trim()}>
                  {creating && <Loader2 className="size-4 animate-spin" />}
                  Create organization
                </Button>
              </DialogFooter>
            </form>
          </DialogContent>
        </Dialog>
      </SidebarMenuItem>
    </SidebarMenu>
  );
}
