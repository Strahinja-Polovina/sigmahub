"use client";

import Link from "next/link";
import {
  Server,
  Boxes,
  CreditCard,
  Rocket,
  MoreHorizontal,
  Play,
  ScrollText,
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
import type { ResourceKind, Status } from "@/lib/mock";
import { RESOURCE_KIND_LABELS as KIND_LABELS } from "@/lib/server-catalog.generated";


type OverviewResource = {
  id: string;
  name: string;
  kind: string;
  status: string;
  projectName: string;
  envName: string;
  lastDeployAt: string | Date;
};

type Activity = {
  id: string;
  resourceName: string;
  author: string;
  sha: string;
  durationSec: number;
  status: string;
};

type Billing = {
  amount: number;
  isFree: boolean;
  /** Free allowance, in units. */
  freeTier: number;
  unitPrice: number;
  connected: number;
  /** Weighted total actually billed. */
  units: number;
  currency: string;
};

function formatCurrency(amount: number, currency: string) {
  return new Intl.NumberFormat("en-IE", {
    style: "currency",
    currency,
    maximumFractionDigits: 0,
  }).format(amount);
}

function formatDate(iso: string | Date) {
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

function ResourceActions({ resourceId, resourceName }: { resourceId: string; resourceName: string }) {
  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        render={
          <Button variant="ghost" size="icon-sm" aria-label={`Actions for ${resourceName}`}>
            <MoreHorizontal />
          </Button>
        }
      />
      {/* These used to be toast-only: Deploy/Restart returned a green success
          toast having done nothing, on the exact controls someone reaches for
          during an incident, and no CP stop/restart endpoint exists at all
          (SIGMA-162). Deploy and Logs now navigate to the resource, where the
          real actions live; Restart is gone until there is a backend for it. */}
      <DropdownMenuContent align="end" className="w-44">
        <DropdownMenuItem render={<Link href={`/dashboard/resources/${resourceId}`} />} className="gap-2">
          <Play className="size-4 text-muted-foreground" />
          Deploy…
        </DropdownMenuItem>
        <DropdownMenuItem render={<Link href={`/dashboard/resources/${resourceId}?tab=logs`} />} className="gap-2">
          <ScrollText className="size-4 text-muted-foreground" />
          Logs
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

const DEPLOY_META: Record<string, { label: string; text: string; dot: string }> = {
  running: { label: "Running", text: "text-blue-700", dot: "bg-blue-500" },
  success: { label: "Success", text: "text-emerald-700", dot: "bg-emerald-500" },
  failed: { label: "Failed", text: "text-red-700", dot: "bg-red-500" },
  building: { label: "Building", text: "text-amber-700", dot: "bg-amber-500" },
  queued: { label: "Queued", text: "text-muted-foreground", dot: "bg-muted-foreground" },
};

function DeployStatusBadge({ status }: { status: string }) {
  const meta = DEPLOY_META[status] ?? DEPLOY_META.queued;
  return (
    <span
      className={`inline-flex shrink-0 items-center gap-1.5 rounded-full border border-border bg-card px-2 py-0.5 text-xs font-medium ${meta.text}`}
    >
      <span className={`size-1.5 rounded-full ${meta.dot}`} aria-hidden />
      {meta.label}
    </span>
  );
}

export function Overview({
  orgName,
  connectedServers,
  totalServers,
  runningResources,
  activeDeploys,
  billing,
  resources,
  activity,
}: {
  orgName: string;
  connectedServers: number;
  totalServers: number;
  runningResources: number;
  activeDeploys: number;
  billing: Billing;
  resources: OverviewResource[];
  activity: Activity[];
}) {
  return (
    <div className="flex flex-col gap-6 p-4 md:p-6">
      <div className="flex flex-col gap-1">
        <h1 className="text-xl font-semibold tracking-tight text-foreground">Overview</h1>
        <p className="text-sm text-muted-foreground">Everything running across {orgName}.</p>
      </div>

      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <StatCard
          label="Connected servers"
          value={connectedServers}
          hint={`${totalServers} total · single billing meter`}
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
          value={formatCurrency(billing.amount, billing.currency)}
          hint={
            billing.isFree ? (
              <span className="inline-flex items-center gap-1.5">
                <StatusDot status="running" />
                Free tier · up to {billing.freeTier} units
              </span>
            ) : (
              `${billing.units} × ${formatCurrency(billing.unitPrice, billing.currency)} per unit`
            )
          }
          icon={CreditCard}
        />
        <StatCard label="Active deploys" value={activeDeploys} hint="In progress right now" icon={Rocket} />
      </div>

      <Card>
        <CardHeader className="border-b">
          <CardTitle>Resources</CardTitle>
          <CardDescription>All resources across your projects and environments.</CardDescription>
        </CardHeader>
        <CardContent className="px-0">
          {resources.length === 0 ? (
            <p className="px-4 py-10 text-center text-sm text-muted-foreground">
              No resources deployed yet.
            </p>
          ) : (
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
                {resources.map((r) => (
                  <TableRow key={r.id}>
                    <TableCell className="pl-4 font-medium text-foreground">
                      <span className="inline-flex items-center gap-2">
                        <StatusDot status={r.status as Status} />
                        {r.name}
                      </span>
                    </TableCell>
                    <TableCell className="text-muted-foreground">
                      {r.projectName}
                      <span className="text-muted-foreground/60"> / {r.envName}</span>
                    </TableCell>
                    <TableCell>
                      <Badge variant="outline" className="font-mono">
                        {KIND_LABELS[r.kind as ResourceKind]}
                      </Badge>
                    </TableCell>
                    <TableCell>
                      <StatusBadge status={r.status as Status} />
                    </TableCell>
                    <TableCell className="text-muted-foreground tabular-nums">
                      {formatDate(r.lastDeployAt)}
                    </TableCell>
                    <TableCell className="pr-4 text-right">
                      <ResourceActions resourceId={r.id} resourceName={r.name} />
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Recent activity</CardTitle>
          <CardDescription>Latest deployments across the organization.</CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col divide-y divide-border">
          {activity.length === 0 ? (
            <p className="py-6 text-sm text-muted-foreground">No deploys yet.</p>
          ) : (
            activity.map((a) => (
              <div key={a.id} className="flex items-center gap-3 py-3 first:pt-0 last:pb-0">
                <span className="grid size-8 shrink-0 place-items-center rounded-md bg-muted text-muted-foreground">
                  <ArrowUpRight className="size-4" />
                </span>
                <div className="min-w-0 flex-1">
                  <p className="truncate text-sm text-foreground">
                    <span className="font-medium">{a.resourceName}</span>{" "}
                    <span className="text-muted-foreground">deployed by {a.author}</span>
                  </p>
                  <p className="truncate text-xs text-muted-foreground">
                    <span className="font-mono">{a.sha}</span> · {a.durationSec}s
                  </p>
                </div>
                <DeployStatusBadge status={a.status} />
              </div>
            ))
          )}
        </CardContent>
      </Card>
    </div>
  );
}
