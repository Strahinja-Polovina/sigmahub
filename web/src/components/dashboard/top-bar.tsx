"use client";

import * as React from "react";
import Link from "next/link";
import { usePathname } from "next/navigation";
import { Search } from "lucide-react";

import {
  Breadcrumb,
  BreadcrumbItem,
  BreadcrumbLink,
  BreadcrumbList,
  BreadcrumbPage,
  BreadcrumbSeparator,
} from "@/components/ui/breadcrumb";
import { Separator } from "@/components/ui/separator";
import { SidebarTrigger } from "@/components/ui/sidebar";
import { ThemeToggle } from "@/components/theme-toggle";
import { CommandMenu, useCommandMenu } from "@/components/dashboard/command-menu";
import { UserMenu } from "@/components/dashboard/user-menu";
import type { CommandIndex } from "@/server/queries";

// Human-readable labels for the first known path segments.
const SEGMENT_LABELS: Record<string, string> = {
  dashboard: "Overview",
  projects: "Projects",
  servers: "Servers",
  resources: "Resources",
  billing: "Billing",
  settings: "Settings",
  members: "Members",
  audit: "Audit log",
  environments: "Environments",
};

function labelFor(segment: string) {
  return (
    SEGMENT_LABELS[segment] ??
    segment.replace(/[-_]/g, " ").replace(/\b\w/g, (c) => c.toUpperCase())
  );
}

function useBreadcrumbs() {
  const pathname = usePathname();
  return React.useMemo(() => {
    const segments = pathname.split("/").filter(Boolean);
    // Build cumulative href per segment; the first "dashboard" is the root crumb.
    const crumbs: { label: string; href: string }[] = [];
    let href = "";
    for (const seg of segments) {
      href += `/${seg}`;
      crumbs.push({ label: labelFor(seg), href });
    }
    if (crumbs.length === 0) {
      crumbs.push({ label: "Overview", href: "/dashboard" });
    }
    return crumbs;
  }, [pathname]);
}

export function TopBar({ commandIndex }: { commandIndex: CommandIndex }) {
  const { open, setOpen } = useCommandMenu();
  const crumbs = useBreadcrumbs();

  return (
    <header className="sticky top-0 z-20 flex h-14 shrink-0 items-center gap-2 border-b bg-background/95 px-4 backdrop-blur supports-[backdrop-filter]:bg-background/80">
      <SidebarTrigger className="-ml-1" />
      <Separator orientation="vertical" className="mr-1 h-4" />

      <Breadcrumb>
        <BreadcrumbList>
          {crumbs.map((crumb, i) => {
            const isLast = i === crumbs.length - 1;
            return (
              <React.Fragment key={crumb.href}>
                <BreadcrumbItem>
                  {isLast ? (
                    <BreadcrumbPage>{crumb.label}</BreadcrumbPage>
                  ) : (
                    <BreadcrumbLink render={<Link href={crumb.href} />}>
                      {crumb.label}
                    </BreadcrumbLink>
                  )}
                </BreadcrumbItem>
                {!isLast && <BreadcrumbSeparator />}
              </React.Fragment>
            );
          })}
        </BreadcrumbList>
      </Breadcrumb>

      <div className="flex-1" />

      <button
        type="button"
        onClick={() => setOpen(true)}
        className="inline-flex h-8 items-center gap-2 rounded-lg border border-border bg-background px-2.5 text-sm text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
        aria-label="Open command menu"
      >
        <Search className="size-4" />
        <span className="hidden md:inline">Search…</span>
        <kbd className="ml-2 hidden items-center gap-0.5 rounded border border-border bg-muted px-1.5 font-mono text-[10px] font-medium text-muted-foreground md:inline-flex">
          <span className="text-xs">⌘</span>K
        </kbd>
      </button>

      <ThemeToggle />
      <UserMenu />

      <CommandMenu open={open} onOpenChange={setOpen} index={commandIndex} />
    </header>
  );
}
