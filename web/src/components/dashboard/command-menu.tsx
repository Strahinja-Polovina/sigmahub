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
import {
  getProjects,
  getEnvironments,
  getServers,
  getResourcesByProject,
} from "@/lib/mock";
import { useActiveOrg } from "@/components/dashboard/org-context";

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
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const router = useRouter();
  const { orgId } = useActiveOrg();

  const projects = React.useMemo(() => getProjects(orgId), [orgId]);
  const servers = React.useMemo(() => getServers(orgId), [orgId]);
  const environments = React.useMemo(
    () => projects.flatMap((p) => getEnvironments(p.id).map((e) => ({ env: e, project: p }))),
    [projects]
  );
  const resources = React.useMemo(
    () => projects.flatMap((p) => getResourcesByProject(p.id).map((r) => ({ res: r, project: p }))),
    [projects]
  );

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

        <CommandGroup heading="Projects">
          {projects.map((p) => (
            <CommandItem
              key={p.id}
              value={`project ${p.name} ${p.slug}`}
              onSelect={() => go("/dashboard/projects")}
            >
              <FolderKanban />
              <span>{p.name}</span>
              <span className="ml-auto text-xs text-muted-foreground">Project</span>
            </CommandItem>
          ))}
        </CommandGroup>

        <CommandGroup heading="Environments">
          {environments.map(({ env, project }) => (
            <CommandItem
              key={env.id}
              value={`environment ${project.name} ${env.name}`}
              onSelect={() => go("/dashboard/projects")}
            >
              <Layers />
              <span>
                {project.name}
                <span className="text-muted-foreground"> / {env.name}</span>
              </span>
            </CommandItem>
          ))}
        </CommandGroup>

        <CommandGroup heading="Servers">
          {servers.map((s) => (
            <CommandItem
              key={s.id}
              value={`server ${s.name} ${s.type} ${s.region}`}
              onSelect={() => go("/dashboard/servers")}
            >
              <Server />
              <span>{s.name}</span>
              <span className="ml-auto text-xs text-muted-foreground">{s.type}</span>
            </CommandItem>
          ))}
        </CommandGroup>

        <CommandGroup heading="Resources">
          {resources.map(({ res, project }) => (
            <CommandItem
              key={res.id}
              value={`resource ${res.name} ${res.kind} ${project.name}`}
              onSelect={() => go("/dashboard/resources")}
            >
              <Boxes />
              <span>{res.name}</span>
              <span className="ml-auto text-xs text-muted-foreground">{res.kind}</span>
            </CommandItem>
          ))}
        </CommandGroup>

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
