"use client";

import * as React from "react";
import Link from "next/link";
import { useParams } from "next/navigation";
import { ArrowUpRight, FolderX, Layers, Server as ServerIcon } from "lucide-react";

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
import { useActiveOrg } from "@/components/dashboard/org-context";
import {
  getDeployments,
  getEnvironments,
  getProject,
  getResources,
  getServer,
} from "@/lib/mock";
import type { DeployStatus, Environment, Resource } from "@/lib/mock";
import { KindBadge, ServerTypeBadge, formatDate, formatDateTime } from "./shared";
import { NewEnvironmentDialog } from "./new-environment-dialog";

function DeployStatusChip({ status }: { status: DeployStatus }) {
  const map: Record<DeployStatus, { label: string; text: string; dot: string }> =
    {
      running: { label: "Running", text: "text-blue-700", dot: "bg-blue-500" },
      success: {
        label: "Success",
        text: "text-emerald-700",
        dot: "bg-emerald-500",
      },
      failed: { label: "Failed", text: "text-red-700", dot: "bg-red-500" },
      building: { label: "Building", text: "text-amber-700", dot: "bg-amber-500" },
    };
  const meta = map[status];
  return (
    <span
      className={`inline-flex shrink-0 items-center gap-1.5 rounded-full border border-border bg-card px-2 py-0.5 text-xs font-medium ${meta.text}`}
    >
      <span className={`size-1.5 rounded-full ${meta.dot}`} aria-hidden />
      {meta.label}
    </span>
  );
}

function AttachedServers({ env }: { env: Environment }) {
  const servers = env.serverIds
    .map((id) => getServer(id))
    .filter((s): s is NonNullable<typeof s> => Boolean(s));

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-sm">
          <ServerIcon className="size-4 text-muted-foreground" />
          Attached servers
        </CardTitle>
        <CardDescription>
          Servers backing resources in this environment.
        </CardDescription>
      </CardHeader>
      <CardContent>
        {servers.length === 0 ? (
          <p className="text-sm text-muted-foreground">
            No servers attached yet.
          </p>
        ) : (
          <ul className="flex flex-col divide-y divide-border">
            {servers.map((server) => (
              <li
                key={server.id}
                className="flex flex-wrap items-center gap-x-3 gap-y-1 py-2.5 first:pt-0 last:pb-0"
              >
                <StatusDot status={server.status} />
                <span className="font-medium text-foreground">
                  {server.name}
                </span>
                <ServerTypeBadge type={server.type} />
                <span className="ml-auto text-xs text-muted-foreground">
                  {server.region}
                </span>
              </li>
            ))}
          </ul>
        )}
      </CardContent>
    </Card>
  );
}

function ResourcesSummary({ resources }: { resources: Resource[] }) {
  return (
    <Card className="lg:col-span-2">
      <CardHeader className="border-b">
        <CardTitle className="text-sm">Resources</CardTitle>
        <CardDescription>
          Apps, databases and services deployed here.
        </CardDescription>
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
                      <StatusDot status={r.status} />
                      {r.name}
                    </span>
                  </TableCell>
                  <TableCell>
                    <KindBadge kind={r.kind} />
                  </TableCell>
                  <TableCell>
                    <StatusBadge status={r.status} />
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

function RecentDeploys({ resources }: { resources: Resource[] }) {
  // Flatten the latest deployment for each resource, newest first.
  const activity = React.useMemo(() => {
    return resources
      .map((r) => ({ resource: r, deployment: getDeployments(r.id)[0] }))
      .filter((a) => a.deployment)
      .sort(
        (a, b) =>
          new Date(b.deployment.startedAt).getTime() -
          new Date(a.deployment.startedAt).getTime()
      )
      .slice(0, 6);
  }, [resources]);

  return (
    <Card className="lg:col-span-3">
      <CardHeader>
        <CardTitle className="text-sm">Recent deploys</CardTitle>
        <CardDescription>
          Latest deployments in this environment.
        </CardDescription>
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
                    <span className="text-muted-foreground">
                      deployed by {deployment.author}
                    </span>
                  </p>
                  <p className="truncate text-xs text-muted-foreground">
                    <span className="font-mono">{deployment.sha}</span> ·{" "}
                    {deployment.durationSec}s · {formatDateTime(deployment.startedAt)}
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

function EnvironmentPanel({ env }: { env: Environment }) {
  const resources = React.useMemo(() => getResources(env.id), [env.id]);
  return (
    <div className="grid grid-cols-1 gap-4 pt-4 lg:grid-cols-3">
      <AttachedServers env={env} />
      <ResourcesSummary resources={resources} />
      <RecentDeploys resources={resources} />
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
            <p className="text-sm font-medium text-foreground">
              Project not found
            </p>
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

export function ProjectDetailView() {
  const params = useParams<{ projectId: string }>();
  const projectId = params.projectId;
  const { orgId } = useActiveOrg();

  const project = React.useMemo(
    () => getProject(projectId),
    [projectId]
  );
  const environments = React.useMemo(
    () => (project ? getEnvironments(project.id) : []),
    [project]
  );

  // Guard: missing project, or one that belongs to a different active org.
  if (!project || project.orgId !== orgId) {
    return <NotFound />;
  }

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
              {project.description}
            </p>
          </div>
          <NewEnvironmentDialog projectName={project.name} />
        </div>

        <div className="flex items-center gap-4 text-xs text-muted-foreground">
          <span className="inline-flex items-center gap-1.5">
            <Layers className="size-3.5" />
            <span className="tabular-nums text-foreground">
              {environments.length}
            </span>
            environments
          </span>
          <span className="font-mono text-muted-foreground/70">
            {project.slug}
          </span>
        </div>
      </div>

      <Separator />

      {environments.length === 0 ? (
        <Card>
          <CardContent className="flex flex-col items-center gap-3 py-12 text-center">
            <span className="grid size-10 place-items-center rounded-lg bg-muted text-muted-foreground">
              <Layers className="size-5" />
            </span>
            <div className="flex flex-col gap-1">
              <p className="text-sm font-medium text-foreground">
                No environments yet
              </p>
              <p className="text-sm text-muted-foreground">
                Add an environment to attach servers and deploy resources.
              </p>
            </div>
            <NewEnvironmentDialog projectName={project.name} />
          </CardContent>
        </Card>
      ) : (
        <Tabs defaultValue={environments[0].id}>
          <TabsList variant="line" className="flex-wrap">
            {environments.map((env) => (
              <TabsTrigger key={env.id} value={env.id} className="capitalize">
                {env.name}
              </TabsTrigger>
            ))}
          </TabsList>
          {environments.map((env) => (
            <TabsContent key={env.id} value={env.id}>
              <EnvironmentPanel env={env} />
            </TabsContent>
          ))}
        </Tabs>
      )}
    </div>
  );
}
