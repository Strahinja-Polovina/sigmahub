"use client";

import * as React from "react";
import Link from "next/link";
import { toast } from "sonner";
import {
  Rocket,
  MoreHorizontal,
  Play,
  ScrollText,
  RotateCw,
  ExternalLink,
  Boxes,
  ListFilter,
} from "lucide-react";

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
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { StatusBadge, StatusDot } from "@/components/dashboard/status-indicator";
import { useActiveOrg } from "@/components/dashboard/org-context";
import {
  getProjects,
  getProject,
  getEnvironment,
  getResourcesByProject,
} from "@/lib/mock";
import type { Resource, ResourceKind } from "@/lib/mock";
import { KIND_LABELS, formatDate } from "./resource-meta";
import { DeployWizard } from "./deploy-wizard";

const ALL = "__all__";

function ResourceActions({ resource }: { resource: Resource }) {
  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        render={
          <Button
            variant="ghost"
            size="icon-sm"
            aria-label={`Actions for ${resource.name}`}
          >
            <MoreHorizontal />
          </Button>
        }
      />
      <DropdownMenuContent align="end" className="w-44">
        <DropdownMenuItem
          className="gap-2"
          onClick={() => toast.success(`Deploy triggered for ${resource.name}`)}
        >
          <Play className="size-4 text-muted-foreground" />
          Deploy
        </DropdownMenuItem>
        <DropdownMenuItem
          className="gap-2"
          onClick={() => toast(`Streaming logs for ${resource.name}…`)}
        >
          <ScrollText className="size-4 text-muted-foreground" />
          View logs
        </DropdownMenuItem>
        <DropdownMenuItem
          className="gap-2"
          onClick={() => toast.success(`Restarting ${resource.name}…`)}
        >
          <RotateCw className="size-4 text-muted-foreground" />
          Restart
        </DropdownMenuItem>
        <DropdownMenuSeparator />
        <DropdownMenuItem
          className="gap-2"
          disabled={!resource.domain}
          onClick={() =>
            resource.domain &&
            toast(`Opening https://${resource.domain}`)
          }
        >
          <ExternalLink className="size-4 text-muted-foreground" />
          Open
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

export function ResourcesView() {
  const { orgId, org } = useActiveOrg();
  const [wizardOpen, setWizardOpen] = React.useState(false);
  const [kindFilter, setKindFilter] = React.useState<string>(ALL);
  const [envFilter, setEnvFilter] = React.useState<string>(ALL);

  const projects = React.useMemo(() => getProjects(orgId), [orgId]);
  const resources = React.useMemo(
    () => projects.flatMap((p) => getResourcesByProject(p.id)),
    [projects]
  );

  // Reset filters if the active org changes (their env ids no longer apply).
  React.useEffect(() => {
    setKindFilter(ALL);
    setEnvFilter(ALL);
  }, [orgId]);

  // Distinct kinds and environments present in this org, for the filter options.
  const kindOptions = React.useMemo(() => {
    const set = new Set<ResourceKind>(resources.map((r) => r.kind));
    return Array.from(set);
  }, [resources]);

  const envOptions = React.useMemo(() => {
    const seen = new Map<string, string>();
    for (const r of resources) {
      if (seen.has(r.environmentId)) continue;
      const env = getEnvironment(r.environmentId);
      const project = getProject(r.projectId);
      if (env && project) {
        seen.set(r.environmentId, `${project.name} / ${env.name}`);
      }
    }
    return Array.from(seen, ([id, label]) => ({ id, label }));
  }, [resources]);

  const filtered = React.useMemo(() => {
    return resources.filter((r) => {
      if (kindFilter !== ALL && r.kind !== kindFilter) return false;
      if (envFilter !== ALL && r.environmentId !== envFilter) return false;
      return true;
    });
  }, [resources, kindFilter, envFilter]);

  return (
    <div className="flex flex-col gap-6 p-4 md:p-6">
      <div className="flex flex-col gap-4 sm:flex-row sm:items-start sm:justify-between">
        <div className="flex flex-col gap-1">
          <h1 className="text-xl font-semibold tracking-tight text-foreground">
            Resources
          </h1>
          <p className="text-sm text-muted-foreground">
            Apps, databases and services running across {org.name}.
          </p>
        </div>
        <Button onClick={() => setWizardOpen(true)}>
          <Rocket className="size-4" />
          Deploy
        </Button>
      </div>

      <Card>
        <CardHeader className="border-b">
          <div className="flex flex-col gap-3 @lg/card-header:flex-row @lg/card-header:items-center @lg/card-header:justify-between">
            <div className="flex flex-col gap-1">
              <CardTitle>All resources</CardTitle>
              <CardDescription>
                {filtered.length} of {resources.length} resources
              </CardDescription>
            </div>
            <div className="flex flex-wrap items-center gap-2">
              <ListFilter className="size-4 text-muted-foreground" />
              <Select
                value={kindFilter}
                onValueChange={(v) => setKindFilter(v as string)}
              >
                <SelectTrigger size="sm" className="w-[140px]">
                  <SelectValue placeholder="Type">
                    {(v) =>
                      v === ALL
                        ? "All types"
                        : KIND_LABELS[v as ResourceKind]
                    }
                  </SelectValue>
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value={ALL}>All types</SelectItem>
                  {kindOptions.map((k) => (
                    <SelectItem key={k} value={k}>
                      {KIND_LABELS[k]}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <Select
                value={envFilter}
                onValueChange={(v) => setEnvFilter(v as string)}
              >
                <SelectTrigger size="sm" className="w-[190px]">
                  <SelectValue placeholder="Environment">
                    {(v) =>
                      v === ALL
                        ? "All environments"
                        : (envOptions.find((e) => e.id === v)?.label ??
                          "All environments")
                    }
                  </SelectValue>
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value={ALL}>All environments</SelectItem>
                  {envOptions.map((e) => (
                    <SelectItem key={e.id} value={e.id}>
                      {e.label}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          </div>
        </CardHeader>
        <CardContent className="px-0">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="pl-4">Name</TableHead>
                <TableHead>Project / Environment</TableHead>
                <TableHead>Type</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Last deploy</TableHead>
                <TableHead className="w-10 pr-4 text-right">
                  <span className="sr-only">Actions</span>
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {filtered.map((r) => {
                const project = getProject(r.projectId);
                const env = getEnvironment(r.environmentId);
                return (
                  <TableRow key={r.id}>
                    <TableCell className="pl-4 font-medium text-foreground">
                      <Link
                        href={`/dashboard/resources/${r.id}`}
                        className="inline-flex items-center gap-2 hover:underline"
                      >
                        <StatusDot status={r.status} />
                        {r.name}
                      </Link>
                    </TableCell>
                    <TableCell className="text-muted-foreground">
                      {project?.name}
                      <span className="text-muted-foreground/60">
                        {" "}
                        / {env?.name}
                      </span>
                    </TableCell>
                    <TableCell>
                      <Badge variant="outline" className="font-mono">
                        {KIND_LABELS[r.kind]}
                      </Badge>
                    </TableCell>
                    <TableCell>
                      <StatusBadge status={r.status} />
                    </TableCell>
                    <TableCell className="text-muted-foreground tabular-nums">
                      {formatDate(r.lastDeployAt)}
                    </TableCell>
                    <TableCell className="pr-4 text-right">
                      <ResourceActions resource={r} />
                    </TableCell>
                  </TableRow>
                );
              })}
              {filtered.length === 0 && (
                <TableRow className="hover:bg-transparent">
                  <TableCell colSpan={6} className="py-12 text-center">
                    <div className="flex flex-col items-center gap-2 text-muted-foreground">
                      <Boxes className="size-6" />
                      <p className="text-sm">No resources match these filters.</p>
                    </div>
                  </TableCell>
                </TableRow>
              )}
            </TableBody>
          </Table>
        </CardContent>
      </Card>

      <DeployWizard
        open={wizardOpen}
        onOpenChange={setWizardOpen}
        orgId={orgId}
      />
    </div>
  );
}
