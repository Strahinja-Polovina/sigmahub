"use client";

import * as React from "react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { ArrowUpRight, FolderX, Layers, Loader2, Server as ServerIcon, Trash2 } from "lucide-react";
import { toast } from "sonner";

import {
  Breadcrumb,
  BreadcrumbItem,
  BreadcrumbList,
  BreadcrumbPage,
  BreadcrumbSeparator,
} from "@/components/ui/breadcrumb";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Separator } from "@/components/ui/separator";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { StatusBadge, StatusDot } from "@/components/dashboard/status-indicator";
import type { ResourceKind, ServerType, Status } from "@/lib/mock";
import type { EnvPanel } from "@/server/queries";
import { deleteEnvironment } from "@/server/actions/projects";
import { KindBadge, ServerTypeBadge, formatDate, formatDateTime } from "./shared";
import { NewEnvironmentDialog } from "./new-environment-dialog";

type ProjectRow = { id: string; name: string; slug: string; description: string };

const DEPLOY_META: Record<string, { label: string; text: string; dot: string }> = {
  running: { label: "Running", text: "text-blue-700", dot: "bg-blue-500" },
  success: { label: "Success", text: "text-emerald-700", dot: "bg-emerald-500" },
  failed: { label: "Failed", text: "text-red-700", dot: "bg-red-500" },
  building: { label: "Building", text: "text-amber-700", dot: "bg-amber-500" },
  queued: { label: "Queued", text: "text-muted-foreground", dot: "bg-muted-foreground" },
};

function DeployStatusChip({ status }: { status: string }) {
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

function AttachedServers({ servers }: { servers: EnvPanel["servers"] }) {
  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-sm">
          <ServerIcon className="size-4 text-muted-foreground" />
          Attached servers
        </CardTitle>
        <CardDescription>Servers backing resources in this environment.</CardDescription>
      </CardHeader>
      <CardContent>
        {servers.length === 0 ? (
          <p className="text-sm text-muted-foreground">No servers attached yet.</p>
        ) : (
          <ul className="flex flex-col divide-y divide-border">
            {servers.map((server) => (
              <li
                key={server.id}
                className="flex flex-wrap items-center gap-x-3 gap-y-1 py-2.5 first:pt-0 last:pb-0"
              >
                <StatusDot status={server.status as Status} />
                <span className="font-medium text-foreground">{server.name}</span>
                <ServerTypeBadge type={server.type as ServerType} />
                <span className="ml-auto text-xs text-muted-foreground">{server.region}</span>
              </li>
            ))}
          </ul>
        )}
      </CardContent>
    </Card>
  );
}

function ResourcesSummary({ resources }: { resources: EnvPanel["resources"] }) {
  return (
    <Card className="lg:col-span-2">
      <CardHeader className="border-b">
        <CardTitle className="text-sm">Resources</CardTitle>
        <CardDescription>Apps, databases and services deployed here.</CardDescription>
      </CardHeader>
      <CardContent className="px-0">
        {resources.length === 0 ? (
          <p className="px-4 py-6 text-sm text-muted-foreground">
            No resources deployed in this environment.
          </p>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="pl-4">Name</TableHead>
                <TableHead>Type</TableHead>
                <TableHead>Status</TableHead>
                <TableHead className="pr-4">Last deploy</TableHead>
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
                  <TableCell>
                    <KindBadge kind={r.kind as ResourceKind} />
                  </TableCell>
                  <TableCell>
                    <StatusBadge status={r.status as Status} />
                  </TableCell>
                  <TableCell className="pr-4 text-muted-foreground tabular-nums">
                    {formatDate(r.lastDeployAt)}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </CardContent>
    </Card>
  );
}

function RecentDeploys({ resources }: { resources: EnvPanel["resources"] }) {
  const activity = resources
    .map((r) => ({ resource: r, deployment: r.latestDeploy }))
    .filter((a): a is { resource: (typeof resources)[number]; deployment: NonNullable<(typeof resources)[number]["latestDeploy"]> } =>
      Boolean(a.deployment)
    )
    .sort(
      (a, b) =>
        new Date(b.deployment.startedAt).getTime() -
        new Date(a.deployment.startedAt).getTime()
    )
    .slice(0, 6);

  return (
    <Card className="lg:col-span-3">
      <CardHeader>
        <CardTitle className="text-sm">Recent deploys</CardTitle>
        <CardDescription>Latest deployments in this environment.</CardDescription>
      </CardHeader>
      <CardContent>
        {activity.length === 0 ? (
          <p className="text-sm text-muted-foreground">No deploys yet.</p>
        ) : (
          <div className="flex flex-col divide-y divide-border">
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
                    <span className="text-muted-foreground">deployed by {deployment.author}</span>
                  </p>
                  <p className="truncate text-xs text-muted-foreground">
                    <span className="font-mono">{deployment.sha}</span> · {deployment.durationSec}s ·{" "}
                    {formatDateTime(deployment.startedAt)}
                  </p>
                </div>
                <DeployStatusChip status={deployment.status} />
              </div>
            ))}
          </div>
        )}
      </CardContent>
    </Card>
  );
}

function DeleteEnvironmentButton({
  environmentId,
  name,
}: {
  environmentId: string;
  name: string;
}) {
  const [open, setOpen] = React.useState(false);
  const [pending, startTransition] = React.useTransition();

  function confirm() {
    startTransition(async () => {
      try {
        await deleteEnvironment({ environmentId });
        toast.success(`Environment “${name}” removed`);
        setOpen(false);
      } catch (err) {
        toast.error("Couldn’t remove environment", {
          description: err instanceof Error ? err.message : "Please try again.",
        });
      }
    });
  }

  return (
    <>
      <Button
        variant="ghost"
        size="sm"
        className="text-muted-foreground hover:text-destructive"
        onClick={() => setOpen(true)}
      >
        <Trash2 className="size-4" />
        Remove
      </Button>
      <Dialog open={open} onOpenChange={(next) => !pending && setOpen(next)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Remove “{name}”?</DialogTitle>
            <DialogDescription>
              This removes the environment and its resources and deployment history.
              Servers stay connected. This can’t be undone.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <DialogClose render={<Button variant="outline" type="button" disabled={pending} />}>
              Cancel
            </DialogClose>
            <Button variant="destructive" onClick={confirm} disabled={pending}>
              {pending && <Loader2 className="size-4 animate-spin" />}
              Remove environment
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  );
}

function EnvironmentPanel({ panel }: { panel: EnvPanel }) {
  return (
    <div className="flex flex-col gap-4 pt-4">
      <div className="flex items-center justify-end">
        <DeleteEnvironmentButton environmentId={panel.env.id} name={panel.env.name} />
      </div>
      <div className="grid grid-cols-1 gap-4 lg:grid-cols-3">
        <AttachedServers servers={panel.servers} />
        <ResourcesSummary resources={panel.resources} />
        <RecentDeploys resources={panel.resources} />
      </div>
    </div>
  );
}

function NotFound() {
  return (
    <div className="flex flex-col gap-6 p-4 md:p-6">
      <Card>
        <CardContent className="flex flex-col items-center gap-3 py-12 text-center">
          <span className="grid size-10 place-items-center rounded-lg bg-muted text-muted-foreground">
            <FolderX className="size-5" />
          </span>
          <div className="flex flex-col gap-1">
            <p className="text-sm font-medium text-foreground">Project not found</p>
            <p className="text-sm text-muted-foreground">
              It may have been removed, or belong to a different organization.
            </p>
          </div>
          <Button
            variant="outline"
            size="sm"
            nativeButton={false}
            render={<Link href="/dashboard/projects" />}
          >
            Back to projects
          </Button>
        </CardContent>
      </Card>
    </div>
  );
}

export function ProjectDetailView({
  project,
  panels,
}: {
  project: ProjectRow | null;
  panels: EnvPanel[];
}) {
  if (!project) return <NotFound />;

  return (
    <div className="flex flex-col gap-6 p-4 md:p-6">
      <div className="flex flex-col gap-3">
        <Breadcrumb>
          <BreadcrumbList>
            <BreadcrumbItem>
              <Link
                href="/dashboard/projects"
                className="transition-colors hover:text-foreground"
              >
                Projects
              </Link>
            </BreadcrumbItem>
            <BreadcrumbSeparator />
            <BreadcrumbItem>
              <BreadcrumbPage>{project.name}</BreadcrumbPage>
            </BreadcrumbItem>
          </BreadcrumbList>
        </Breadcrumb>

        <div className="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
          <div className="flex flex-col gap-1">
            <h1 className="text-xl font-semibold tracking-tight text-foreground">
              {project.name}
            </h1>
            <p className="text-sm text-muted-foreground">
              {project.description || "No description"}
            </p>
          </div>
          <NewEnvironmentDialog projectId={project.id} projectName={project.name} />
        </div>

        <div className="flex items-center gap-4 text-xs text-muted-foreground">
          <span className="inline-flex items-center gap-1.5">
            <Layers className="size-3.5" />
            <span className="tabular-nums text-foreground">{panels.length}</span>
            environments
          </span>
          <span className="font-mono text-muted-foreground/70">{project.slug}</span>
        </div>
      </div>

      <Separator />

      {panels.length === 0 ? (
        <Card>
          <CardContent className="flex flex-col items-center gap-3 py-12 text-center">
            <span className="grid size-10 place-items-center rounded-lg bg-muted text-muted-foreground">
              <Layers className="size-5" />
            </span>
            <div className="flex flex-col gap-1">
              <p className="text-sm font-medium text-foreground">No environments yet</p>
              <p className="text-sm text-muted-foreground">
                Add an environment to attach servers and deploy resources.
              </p>
            </div>
            <NewEnvironmentDialog projectId={project.id} projectName={project.name} />
          </CardContent>
        </Card>
      ) : (
        <Tabs defaultValue={panels[0].env.id}>
          <TabsList variant="line" className="flex-wrap">
            {panels.map((panel) => (
              <TabsTrigger key={panel.env.id} value={panel.env.id} className="capitalize">
                {panel.env.name}
              </TabsTrigger>
            ))}
          </TabsList>
          {panels.map((panel) => (
            <TabsContent key={panel.env.id} value={panel.env.id}>
              <EnvironmentPanel panel={panel} />
            </TabsContent>
          ))}
        </Tabs>
      )}
    </div>
  );
}
