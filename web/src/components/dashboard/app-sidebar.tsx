"use client";

import * as React from "react";
import Link from "next/link";
import { usePathname } from "next/navigation";
import {
  LayoutDashboard,
  FolderKanban,
  Server,
  Boxes,
  CreditCard,
  Settings,
  ChevronRight,
  Layers,
} from "lucide-react";

import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarGroup,
  SidebarGroupLabel,
  SidebarHeader,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarMenuSub,
  SidebarMenuSubButton,
  SidebarMenuSubItem,
} from "@/components/ui/sidebar";
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from "@/components/ui/collapsible";
import { Logo } from "@/components/logo";
import { OrgSwitcher } from "@/components/dashboard/org-switcher";

export type SidebarProject = {
  id: string;
  name: string;
  environments: { id: string; name: string }[];
};

const PRIMARY_NAV: { label: string; href: string; icon: React.ElementType }[] = [
  { label: "Overview", href: "/dashboard", icon: LayoutDashboard },
  { label: "Projects", href: "/dashboard/projects", icon: FolderKanban },
  { label: "Servers", href: "/dashboard/servers", icon: Server },
  { label: "Resources", href: "/dashboard/resources", icon: Boxes },
  { label: "Billing", href: "/dashboard/billing", icon: CreditCard },
  { label: "Settings", href: "/dashboard/settings", icon: Settings },
];

function isActivePath(pathname: string, href: string) {
  if (href === "/dashboard") return pathname === "/dashboard";
  return pathname === href || pathname.startsWith(`${href}/`);
}

// Controlled Collapsible per project — Base UI warns if an uncontrolled
// Collapsible's defaultOpen changes across renders (it does, on navigation).
function ProjectNavItem({
  project,
  pathname,
}: {
  project: SidebarProject;
  pathname: string;
}) {
  const environments = project.environments;
  const projectActive = pathname.startsWith(
    `/dashboard/projects/${project.id}`
  );
  const [open, setOpen] = React.useState(projectActive);

  // Auto-open when this project becomes active (e.g. via ⌘K or a direct link).
  React.useEffect(() => {
    if (projectActive) setOpen(true);
  }, [projectActive]);

  return (
    <Collapsible open={open} onOpenChange={setOpen} render={<SidebarMenuItem />}>
      <CollapsibleTrigger
        render={
          <SidebarMenuButton
            tooltip={project.name}
            className="group/collapsible-trigger"
          >
            <FolderKanban />
            <span>{project.name}</span>
            <ChevronRight className="ml-auto size-4 shrink-0 text-muted-foreground transition-transform group-data-[panel-open]/collapsible-trigger:rotate-90" />
          </SidebarMenuButton>
        }
      />
      <CollapsibleContent>
        <SidebarMenuSub>
          {environments.map((env) => {
            const href = `/dashboard/projects/${project.id}/environments/${env.id}`;
            return (
              <SidebarMenuSubItem key={env.id}>
                <SidebarMenuSubButton
                  isActive={pathname === href}
                  render={<Link href={href} />}
                >
                  <Layers />
                  <span>{env.name}</span>
                </SidebarMenuSubButton>
              </SidebarMenuSubItem>
            );
          })}
        </SidebarMenuSub>
      </CollapsibleContent>
    </Collapsible>
  );
}

export function AppSidebar({ projects }: { projects: SidebarProject[] }) {
  const pathname = usePathname();

  return (
    <Sidebar collapsible="icon">
      <SidebarHeader>
        <OrgSwitcher />
      </SidebarHeader>

      <SidebarContent>
        <SidebarGroup>
          <SidebarGroupLabel>Platform</SidebarGroupLabel>
          <SidebarMenu>
            {PRIMARY_NAV.map((item) => {
              const active = isActivePath(pathname, item.href);
              return (
                <SidebarMenuItem key={item.href}>
                  <SidebarMenuButton
                    isActive={active}
                    tooltip={item.label}
                    render={<Link href={item.href} />}
                  >
                    <item.icon />
                    <span>{item.label}</span>
                  </SidebarMenuButton>
                </SidebarMenuItem>
              );
            })}
          </SidebarMenu>
        </SidebarGroup>

        <SidebarGroup className="group-data-[collapsible=icon]:hidden">
          <SidebarGroupLabel>Projects</SidebarGroupLabel>
          <SidebarMenu>
            {projects.map((project) => (
              <ProjectNavItem
                key={project.id}
                project={project}
                pathname={pathname}
              />
            ))}
          </SidebarMenu>
        </SidebarGroup>
      </SidebarContent>

      <SidebarFooter className="group-data-[collapsible=icon]:hidden">
        <Link
          href="/dashboard"
          className="flex items-center gap-2 rounded-md px-2 py-1.5 text-xs text-muted-foreground"
        >
          <Logo />
        </Link>
      </SidebarFooter>
    </Sidebar>
  );
}
