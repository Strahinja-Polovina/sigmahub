"use client";

import * as React from "react";
import { useRouter } from "next/navigation";
import {
  LayoutDashboard,
  FolderKanban,
  Server,
  Boxes,
  CreditCard,
  Settings,
  Layers,
} from "lucide-react";

import {
  CommandDialog,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
  CommandSeparator,
} from "@/components/ui/command";
import type { CommandIndex } from "@/server/queries";

const NAV_ITEMS: { label: string; href: string; icon: React.ElementType }[] = [
  { label: "Overview", href: "/dashboard", icon: LayoutDashboard },
  { label: "Projects", href: "/dashboard/projects", icon: FolderKanban },
  { label: "Servers", href: "/dashboard/servers", icon: Server },
  { label: "Resources", href: "/dashboard/resources", icon: Boxes },
  { label: "Billing", href: "/dashboard/billing", icon: CreditCard },
  { label: "Settings", href: "/dashboard/settings", icon: Settings },
];

export function CommandMenu({
  open,
  onOpenChange,
  index,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  index: CommandIndex;
}) {
  const router = useRouter();
  const { projects, environments, servers, resources } = index;

  const go = React.useCallback(
    (href: string) => {
      onOpenChange(false);
      router.push(href);
    },
    [onOpenChange, router]
  );

  return (
    <CommandDialog open={open} onOpenChange={onOpenChange}>
      <CommandInput placeholder="Search projects, servers, resources…" />
      <CommandList>
        <CommandEmpty>No results found.</CommandEmpty>

        {projects.length > 0 && (
          <CommandGroup heading="Projects">
            {projects.map((p) => (
              <CommandItem
                key={p.id}
                value={`project ${p.name} ${p.slug}`}
                onSelect={() => go(`/dashboard/projects/${p.id}`)}
              >
                <FolderKanban />
                <span>{p.name}</span>
                <span className="ml-auto text-xs text-muted-foreground">Project</span>
              </CommandItem>
            ))}
          </CommandGroup>
        )}

        {environments.length > 0 && (
          <CommandGroup heading="Environments">
            {environments.map((e) => (
              <CommandItem
                key={e.id}
                value={`environment ${e.projectName} ${e.name}`}
                onSelect={() => go(`/dashboard/projects/${e.projectId}/environments/${e.id}`)}
              >
                <Layers />
                <span>
                  {e.projectName}
                  <span className="text-muted-foreground"> / {e.name}</span>
                </span>
              </CommandItem>
            ))}
          </CommandGroup>
        )}

        {servers.length > 0 && (
          <CommandGroup heading="Servers">
            {servers.map((sv) => (
              <CommandItem
                key={sv.id}
                value={`server ${sv.name} ${sv.type} ${sv.region}`}
                onSelect={() => go(`/dashboard/servers/${sv.id}`)}
              >
                <Server />
                <span>{sv.name}</span>
                <span className="ml-auto text-xs text-muted-foreground">{sv.type}</span>
              </CommandItem>
            ))}
          </CommandGroup>
        )}

        {resources.length > 0 && (
          <CommandGroup heading="Resources">
            {resources.map((r) => (
              <CommandItem
                key={r.id}
                value={`resource ${r.name} ${r.kind} ${r.projectName}`}
                onSelect={() => go(`/dashboard/resources/${r.id}`)}
              >
                <Boxes />
                <span>{r.name}</span>
                <span className="ml-auto text-xs text-muted-foreground">{r.kind}</span>
              </CommandItem>
            ))}
          </CommandGroup>
        )}

        <CommandSeparator />

        <CommandGroup heading="Navigation">
          {NAV_ITEMS.map((item) => (
            <CommandItem
              key={item.href}
              value={`go to ${item.label}`}
              onSelect={() => go(item.href)}
            >
              <item.icon />
              <span>{item.label}</span>
            </CommandItem>
          ))}
        </CommandGroup>
      </CommandList>
    </CommandDialog>
  );
}

// Hook that wires ⌘K / Ctrl+K and exposes open state for external triggers.
export function useCommandMenu() {
  const [open, setOpen] = React.useState(false);

  React.useEffect(() => {
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === "k" && (e.metaKey || e.ctrlKey)) {
        e.preventDefault();
        setOpen((prev) => !prev);
      }
    };
    document.addEventListener("keydown", onKeyDown);
    return () => document.removeEventListener("keydown", onKeyDown);
  }, []);

  return { open, setOpen };
}
