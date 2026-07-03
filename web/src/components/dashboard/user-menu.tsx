"use client";

import Link from "next/link";
import { User, Settings, LogOut } from "lucide-react";

import { Avatar, AvatarFallback } from "@/components/ui/avatar";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { getMembers } from "@/lib/mock";
import { useActiveOrg } from "@/components/dashboard/org-context";

function initials(name: string) {
  return name
    .split(" ")
    .map((p) => p.charAt(0))
    .slice(0, 2)
    .join("")
    .toUpperCase();
}

export function UserMenu() {
  const { orgId } = useActiveOrg();
  // The first member is the signed-in Org Admin in this prototype.
  const user = getMembers(orgId)[0];

  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        render={
          <Button
            variant="ghost"
            size="icon"
            className="rounded-full"
            aria-label="Account menu"
          >
            <Avatar size="sm">
              <AvatarFallback>{initials(user.name)}</AvatarFallback>
            </Avatar>
          </Button>
        }
      />
      <DropdownMenuContent align="end" side="bottom" sideOffset={8} className="w-56">
        <div className="flex flex-col px-2 py-1.5">
          <span className="truncate text-sm font-medium text-foreground">
            {user.name}
          </span>
          <span className="truncate text-xs text-muted-foreground">
            {user.email}
          </span>
        </div>
        <DropdownMenuSeparator />
        <DropdownMenuGroup>
          <DropdownMenuItem render={<Link href="/dashboard/settings" />} className="gap-2">
            <User className="size-4 text-muted-foreground" />
            Profile
          </DropdownMenuItem>
          <DropdownMenuItem render={<Link href="/dashboard/settings" />} className="gap-2">
            <Settings className="size-4 text-muted-foreground" />
            Settings
          </DropdownMenuItem>
        </DropdownMenuGroup>
        <DropdownMenuSeparator />
        <DropdownMenuItem
          variant="destructive"
          render={<Link href="/login" />}
          className="gap-2"
        >
          <LogOut className="size-4" />
          Log out
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
