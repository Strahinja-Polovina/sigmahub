"use client";

import * as React from "react";
import { toast } from "sonner";
import {
  Server,
  Boxes,
  CreditCard,
  Rocket,
  MoreHorizontal,
  Play,
  ScrollText,
  RotateCw,
  ArrowUpRight,
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
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { StatusBadge, StatusDot } from "@/components/dashboard/status-indicator";
import { useActiveOrg } from "@/components/dashboard/org-context";
import {
  getServers,
  getProjects,
  getProject,
  getEnvironment,
  getResourcesByProject,
  getBillingSummary,
  getDeployments,
  CURRENCY,
} from "@/lib/mock";
import type { Resource, ResourceKind } from "@/lib/mock";

const KIND_LABELS: Record<ResourceKind, string> = {
  app: "App",
  postgres: "PostgreSQL",
  mysql: "MySQL",
  mongo: "MongoDB",
  redis: "Redis",
  s3: "Object storage",
  llm: "LLM",
};

function formatCurrency(amount: number) {
  return new Intl.NumberFormat("en-IE", {
    style: "currency",
    currency: CURRENCY,
    maximumFractionDigits: 0,
  }).format(amount);
}

function formatDate(iso: string) {
  return new Date(iso).toLocaleDateString("en-GB", {
    day: "numeric",
    month: "short",
  });
}

function StatCard({
  label,
  value,
  hint,
  icon: Icon,
}: {
  label: string;
  value: React.ReactNode;
  hint?: React.ReactNode;
  icon: React.ElementType;
}) {
  return (
    <Card size="sm">
      <CardHeader>
        <CardDescription className="flex items-center justify-between">
          <span>{label}</span>
          <Icon className="size-4 text-muted-foreground" />
        </CardDescription>
        <CardTitle className="text-2xl tabular-nums">{value}</CardTitle>
      </CardHeader>
      {hint && (
        <CardContent className="text-xs text-muted-foreground">{hint}</CardContent>
      )}
    </Card>
  );
}

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
      <DropdownMenuContent align="end" className="w-40">
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
          Logs
        </DropdownMenuItem>
        <DropdownMenuItem
          className="gap-2"
          onClick={() => toast.success(`Restarting ${resource.name}…`)}
        >
          <RotateCw className="size-4 text-muted-foreground" />
          Restart
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

export function Overview() {
  const { orgId, org } = useActiveOrg();

  const servers = React.useMemo(() => getServers(orgId), [orgId]);
  const projects = React.useMemo(() => getProjects(orgId), [orgId]);
  const resources = React.useMemo(
    () => projects.flatMap((p) => getResourcesByProject(p.id)),
    [projects]
  );
  const billing = React.useMemo(() => getBillingSummary(orgId), [orgId]);

  const connectedServers = React.useMemo(
    () => servers.filter((s) => s.status !== "provisioning").length,
    [servers]
  );
  const runningResources = React.useMemo(
    () => resources.filter((r) => r.status === "running").length,
    [resources]
  );

  // Recent deployments across the org's resources, newest first.
  const activity = React.useMemo(() => {
    return resources
      .map((r) => {
        const dep = getDeployments(r.id)[0];
        return { resource: r, deployment: dep };
      })
      .sort(
        (a, b) =>
          new Date(b.deployment.startedAt).getTime() -
          new Date(a.deployment.startedAt).getTime()
      )
      .slice(0, 6);
  }, [resources]);

  const activeDeploys = React.useMemo(
    () =>
      resources.filter((r) => getDeployments(r.id)[0]?.status === "running")
        .length,
    [resources]
  );

  return (
    <div className="flex flex-col gap-6 p-4 md:p-6">
      <div className="flex flex-col gap-1">
        <h1 className="text-xl font-semibold tracking-tight text-foreground">
          Overview
        </h1>
        <p className="text-sm text-muted-foreground">
          Everything running across {org.name}.
        </p>
      </div>

      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <StatCard
          label="Connected servers"
          value={connectedServers}
          hint={`${servers.length} total · single billing meter`}
          icon={Server}
        />
        <StatCard
          label="Running resources"
          value={runningResources}
          hint={`${resources.length} resources deployed`}
          icon={Boxes}
        />
        <StatCard
          label="Monthly cost"
          value={formatCurrency(billing.amount)}
          hint={
            billing.isFree ? (
              <span className="inline-flex items-center gap-1.5">
                <StatusDot status="running" />
                Free tier · up to {billing.freeTier} servers
              </span>
            ) : (
              `${billing.connected} × ${formatCurrency(billing.unitPrice)} per server`
            )
          }
          icon={CreditCard}
        />
        <StatCard
          label="Active deploys"
          value={activeDeploys}
          hint="In progress right now"
          icon={Rocket}
        />
      </div>

      <Card>
        <CardHeader className="border-b">
          <CardTitle>Resources</CardTitle>
          <CardDescription>
            All resources across your projects and environments.
          </CardDescription>
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
              {resources.map((r) => {
                const project = getProject(r.projectId);
                const env = getEnvironment(r.environmentId);
                return (
                  <TableRow key={r.id}>
                    <TableCell className="pl-4 font-medium text-foreground">
                      <span className="inline-flex items-center gap-2">
                        <StatusDot status={r.status} />
                        {r.name}
                      </span>
                    </TableCell>
                    <TableCell className="text-muted-foreground">
                      {project?.name}
                      <span className="text-muted-foreground/60"> / {env?.name}</span>
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
            </TableBody>
          </Table>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Recent activity</CardTitle>
          <CardDescription>Latest deployments across the organization.</CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col divide-y divide-border">
          {activity.map(({ resource, deployment }) => (
            <div
              key={deployment.id}
              className="flex items-center gap-3 py-3 first:pt-0 last:pb-0"
            >
              <span className="grid size-8 shrink-0 place-items-center rounded-md bg-muted text-muted-foreground">
                <ArrowUpRight className="size-4" />
              </span>
              <div className="min-w-0 flex-1">
                <p className="truncate text-sm text-foreground">
                  <span className="font-medium">{resource.name}</span>{" "}
                  <span className="text-muted-foreground">
                    deployed by {deployment.author}
                  </span>
                </p>
                <p className="truncate text-xs text-muted-foreground">
                  <span className="font-mono">{deployment.sha}</span> ·{" "}
                  {deployment.durationSec}s
                </p>
              </div>
              <DeployStatusBadge status={deployment.status} />
            </div>
          ))}
        </CardContent>
      </Card>
    </div>
  );
}

function DeployStatusBadge({
  status,
}: {
  status: ReturnType<typeof getDeployments>[number]["status"];
}) {
  const map: Record<typeof status, { label: string; className: string }> = {
    running: { label: "Running", className: "text-blue-700" },
    success: { label: "Success", className: "text-emerald-700" },
    failed: { label: "Failed", className: "text-red-700" },
    building: { label: "Building", className: "text-amber-700" },
  };
  const meta = map[status];
  const dot: Record<typeof status, string> = {
    running: "bg-blue-500",
    success: "bg-emerald-500",
    failed: "bg-red-500",
    building: "bg-amber-500",
  };
  return (
    <span
      className={`inline-flex shrink-0 items-center gap-1.5 rounded-full border border-border bg-card px-2 py-0.5 text-xs font-medium ${meta.className}`}
    >
      <span className={`size-1.5 rounded-full ${dot[status]}`} aria-hidden />
      {meta.label}
    </span>
  );
}
