"use client";

import * as React from "react";
import Link from "next/link";
import {
  Rocket,
  MoreHorizontal,
  ScrollText,
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
import type { ResourceKind, Status } from "@/lib/mock";
import { KIND_LABELS, formatDate, type DeployTarget } from "./resource-meta";
import { DeployWizard } from "./deploy-wizard";
import type { WizardCluster, WizardServer } from "@/lib/wizard/availability";

const ALL = "__all__";

type ResourceItem = {
  id: string;
  name: string;
  kind: string;
  status: string;
  projectName: string;
  envName: string;
  environmentId: string;
  lastDeployAt: string | Date;
  domain: string | null;
};

function ResourceActions({ resource }: { resource: ResourceItem }) {
  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        render={
          <Button variant="ghost" size="icon-sm" aria-label={`Actions for ${resource.name}`}>
            <MoreHorizontal />
          </Button>
        }
      />
      <DropdownMenuContent align="end" className="w-44">
        <DropdownMenuItem render={<Link href={`/dashboard/resources/${resource.id}`} />} className="gap-2">
          <ScrollText className="size-4 text-muted-foreground" />
          View details
        </DropdownMenuItem>
        {/* A real link to the running app. This was
            `toast(`Opening https://${resource.domain}`)` — a menu item that
            announced a navigation it never performed, leaving the user to
            retype the hostname into a new tab (SIGMA-238). A resource with no
            domain has nowhere to go, so the item is absent rather than present
            and disabled. */}
        {resource.domain && (
          <>
            <DropdownMenuSeparator />
            <DropdownMenuItem
              className="gap-2"
              render={
                <a
                  href={`https://${resource.domain}`}
                  target="_blank"
                  rel="noopener noreferrer"
                />
              }
            >
              <ExternalLink className="size-4 text-muted-foreground" />
              Open
            </DropdownMenuItem>
          </>
        )}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

export function ResourcesView({
  orgName,
  resources,
  targets,
  cpMode = false,
  orgId = "",
  clusters = [],
  clusterExcludedKinds = [],
  /** The whole fleet, so step 1 can tell "no such server" from "one is
   *  connected but attached to no environment" (SIGMA-309). */
  orgServers = [],
  /** The GitHub install round trip lands back here with ?wizard=resume, and the
   *  wizard picks the draft it left in sessionStorage back up (SIGMA-208). */
  resumeWizard = false,
}: {
  orgName: string;
  resources: ResourceItem[];
  targets: DeployTarget[];
  cpMode?: boolean;
  orgId?: string;
  clusters?: WizardCluster[];
  clusterExcludedKinds?: string[];
  orgServers?: WizardServer[];
  resumeWizard?: boolean;
}) {
  const [wizardOpen, setWizardOpen] = React.useState(resumeWizard);
  const [kindFilter, setKindFilter] = React.useState<string>(ALL);
  const [envFilter, setEnvFilter] = React.useState<string>(ALL);

  const kindOptions = React.useMemo(
    () => Array.from(new Set(resources.map((r) => r.kind))),
    [resources]
  );
  const envOptions = React.useMemo(() => {
    const seen = new Map<string, string>();
    for (const r of resources) {
      if (!seen.has(r.environmentId)) {
        seen.set(r.environmentId, `${r.projectName} / ${r.envName}`);
      }
    }
    return Array.from(seen, ([id, label]) => ({ id, label }));
  }, [resources]);

  const filtered = resources.filter((r) => {
    if (kindFilter !== ALL && r.kind !== kindFilter) return false;
    if (envFilter !== ALL && r.environmentId !== envFilter) return false;
    return true;
  });

  return (
    <div className="flex flex-col gap-6 p-4 md:p-6">
      <div className="flex flex-col gap-4 sm:flex-row sm:items-start sm:justify-between">
        <div className="flex flex-col gap-1">
          <h1 className="text-xl font-semibold tracking-tight text-foreground">Resources</h1>
          <p className="text-sm text-muted-foreground">
            Apps, databases and services running across {orgName}.
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
              <Select value={kindFilter} onValueChange={(v) => setKindFilter(v as string)}>
                <SelectTrigger size="sm" className="w-[140px]">
                  <SelectValue placeholder="Type">
                    {(v) => (v === ALL ? "All types" : KIND_LABELS[v as ResourceKind])}
                  </SelectValue>
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value={ALL}>All types</SelectItem>
                  {kindOptions.map((k) => (
                    <SelectItem key={k} value={k}>
                      {KIND_LABELS[k as ResourceKind]}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <Select value={envFilter} onValueChange={(v) => setEnvFilter(v as string)}>
                <SelectTrigger size="sm" className="w-[190px]">
                  <SelectValue placeholder="Environment">
                    {(v) =>
                      v === ALL
                        ? "All environments"
                        : (envOptions.find((e) => e.id === v)?.label ?? "All environments")
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
              {filtered.map((r) => (
                <TableRow key={r.id}>
                  <TableCell className="pl-4 font-medium text-foreground">
                    <Link
                      href={`/dashboard/resources/${r.id}`}
                      className="inline-flex items-center gap-2 hover:underline"
                    >
                      <StatusDot status={r.status as Status} />
                      {r.name}
                    </Link>
                  </TableCell>
                  <TableCell className="text-muted-foreground">
                    {r.projectName}
                    <span className="text-muted-foreground/60"> / {r.envName}</span>
                  </TableCell>
                  <TableCell>
                    <span className="inline-flex items-center gap-1.5">
                      <Badge variant="outline" className="font-mono">
                        {KIND_LABELS[r.kind as ResourceKind]}
                      </Badge>
                      {"ephemeral" in r && Boolean(r.ephemeral) && (
                        <Badge variant="secondary" className="text-[10px]">
                          PR preview
                        </Badge>
                      )}
                    </span>
                  </TableCell>
                  <TableCell>
                    <StatusBadge status={r.status as Status} />
                  </TableCell>
                  <TableCell className="text-muted-foreground tabular-nums">
                    {formatDate(r.lastDeployAt)}
                  </TableCell>
                  <TableCell className="pr-4 text-right">
                    <ResourceActions resource={r} />
                  </TableCell>
                </TableRow>
              ))}
              {filtered.length === 0 && (
                <TableRow className="hover:bg-transparent">
                  <TableCell colSpan={6} className="py-12 text-center">
                    <div className="flex flex-col items-center gap-2 text-muted-foreground">
                      <Boxes className="size-6" />
                      <p className="text-sm">
                        {resources.length === 0
                          ? "No resources yet. Deploy one from Git to get started."
                          : "No resources match these filters."}
                      </p>
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
        targets={targets}
        cpMode={cpMode}
        orgId={orgId}
        clusters={clusters}
        clusterExcludedKinds={clusterExcludedKinds}
        orgServers={orgServers}
        resume={resumeWizard}
      />
    </div>
  );
}
