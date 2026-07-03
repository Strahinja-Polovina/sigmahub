"use client";

import Link from "next/link";
import {
  ChevronsUpDown,
  Check,
  CreditCard,
  Users,
  ScrollText,
  Plus,
} from "lucide-react";

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

function orgInitial(name: string) {
  return name.charAt(0).toUpperCase();
}

export function OrgSwitcher() {
  const { org, orgs, setOrgId } = useActiveOrg();

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
                  onClick={() => setOrgId(o.id)}
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
                render={<Link href="/dashboard/members" />}
                className="gap-2"
              >
                <Users className="size-4 text-muted-foreground" />
                Members
              </DropdownMenuItem>
              <DropdownMenuItem
                render={<Link href="/dashboard/audit" />}
                className="gap-2"
              >
                <ScrollText className="size-4 text-muted-foreground" />
                Audit log
              </DropdownMenuItem>
            </DropdownMenuGroup>
            <DropdownMenuSeparator />
            <DropdownMenuItem
              render={<Link href="/dashboard/settings" />}
              className="gap-2"
            >
              <Plus className="size-4 text-muted-foreground" />
              New organization
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </SidebarMenuItem>
    </SidebarMenu>
  );
}
