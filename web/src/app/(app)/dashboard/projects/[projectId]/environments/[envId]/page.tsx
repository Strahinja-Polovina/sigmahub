"use client";

import Link from "next/link";
import { useParams } from "next/navigation";
import { ArrowLeft } from "lucide-react";

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
import { Button } from "@/components/ui/button";
import { StatusDot, StatusBadge } from "@/components/dashboard/status-indicator";
import {
  KindBadge,
  ServerTypeBadge,
  formatDate,
} from "@/components/dashboard/projects/shared";
import { useActiveOrg } from "@/components/dashboard/org-context";
import {
  getProject,
  getEnvironment,
  getResources,
  getServer,
  getDeployments,
} from "@/lib/mock";
import type { Server } from "@/lib/mock";

export default function EnvironmentDetailPage() {
  const params = useParams<{ projectId: string; envId: string }>();
  const projectId = params.projectId;
  const envId = params.envId;
  const { orgId } = useActiveOrg();

  const project = getProject(projectId);
  const env = getEnvironment(envId);

  // Guard: unknown ids, or an env/project that belongs to another org.
  if (
    !project ||
    !env ||
    project.orgId !== orgId ||
    env.projectId !== projectId
  ) {
    return (
      <div className="p-4 md:p-6">
        <Card className="mx-auto max-w-md text-center">
          <CardHeader>
            <CardTitle>Environment not found</CardTitle>
            <CardDescription>
              It may belong to another organization or no longer exist.
            </CardDescription>
          </CardHeader>
          <CardContent>
            <Button nativeButton={false} render={<Link href="/dashboard/projects" />}>
              <ArrowLeft />
              Back to projects
            </Button>
          </CardContent>
        </Card>
      </div>
    );
  }

  const servers = env.serverIds
    .map((id) => getServer(id))
    .filter((s): s is Server => Boolean(s));
  const resources = getResources(envId);
  const deploys = resources
    .flatMap((r) =>
      getDeployments(r.id)
        .slice(0, 2)
        .map((d) => ({ ...d, resourceName: r.name }))
    )
    .sort((a, b) => (a.startedAt < b.startedAt ? 1 : -1))
    .slice(0, 6);

  return (
    <div className="flex flex-col gap-6 p-4 md:p-6">
      <div className="flex flex-col gap-2">
        <Link
          href={`/dashboard/projects/${project.id}`}
          className="inline-flex w-fit items-center gap-1.5 text-sm text-muted-foreground transition-colors hover:text-foreground"
        >
          <ArrowLeft className="size-3.5" />
          {project.name}
        </Link>
        <div className="flex items-baseline gap-2">
          <h1 className="text-xl font-semibold tracking-tight text-foreground">
            {env.name}
          </h1>
          <span className="text-sm text-muted-foreground">
            · {project.name}
          </span>
        </div>
        <p className="text-sm text-muted-foreground">
          {resources.length} {resources.length === 1 ? "resource" : "resources"}{" "}
          · {servers.length} {servers.length === 1 ? "server" : "servers"}
        </p>
      </div>

      <div className="grid gap-6 lg:grid-cols-3">
        <Card className="lg:col-span-1">
          <CardHeader>
            <CardTitle className="text-base">Attached servers</CardTitle>
          </CardHeader>
          <CardContent className="flex flex-col gap-3">
            {servers.length === 0 ? (
              <p className="text-sm text-muted-foreground">No servers attached.</p>
            ) : (
              servers.map((s) => (
                <div
                  key={s.id}
                  className="flex items-center justify-between gap-2"
                >
                  <Link
                    href={`/dashboard/servers/${s.id}`}
                    className="inline-flex items-center gap-2 text-sm font-medium text-foreground hover:underline"
                  >
                    <StatusDot status={s.status} />
                    {s.name}
                  </Link>
                  <ServerTypeBadge type={s.type} />
                </div>
              ))
            )}
          </CardContent>
        </Card>

        <Card className="lg:col-span-2">
          <CardHeader>
            <CardTitle className="text-base">Resources</CardTitle>
          </CardHeader>
          <CardContent className="px-0">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead className="pl-6">Name</TableHead>
                  <TableHead>Type</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead className="pr-6">Last deploy</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {resources.length === 0 ? (
                  <TableRow>
                    <TableCell
                      colSpan={4}
                      className="py-8 text-center text-sm text-muted-foreground"
                    >
                      No resources in this environment.
                    </TableCell>
                  </TableRow>
                ) : (
                  resources.map((r) => (
                    <TableRow key={r.id}>
                      <TableCell className="pl-6 font-medium">
                        <Link
                          href={`/dashboard/resources/${r.id}`}
                          className="inline-flex items-center gap-2 hover:underline"
                        >
                          <StatusDot status={r.status} />
                          {r.name}
                        </Link>
                      </TableCell>
                      <TableCell>
                        <KindBadge kind={r.kind} />
                      </TableCell>
                      <TableCell>
                        <StatusBadge status={r.status} />
                      </TableCell>
                      <TableCell className="pr-6 text-muted-foreground">
                        {formatDate(r.lastDeployAt)}
                      </TableCell>
                    </TableRow>
                  ))
                )}
              </TableBody>
            </Table>
          </CardContent>
        </Card>
      </div>

      <Card>
        <CardHeader>
          <CardTitle className="text-base">Recent deploys</CardTitle>
        </CardHeader>
        <CardContent className="flex flex-col gap-3">
          {deploys.length === 0 ? (
            <p className="text-sm text-muted-foreground">No recent deploys.</p>
          ) : (
            deploys.map((d) => (
              <div
                key={d.id}
                className="flex items-center justify-between gap-3 text-sm"
              >
                <div className="flex min-w-0 items-center gap-2">
                  <span className="font-medium text-foreground">
                    {d.resourceName}
                  </span>
                  <span className="font-mono text-xs text-muted-foreground">
                    {d.sha}
                  </span>
                  <span className="truncate text-xs text-muted-foreground">
                    by {d.author}
                  </span>
                </div>
                <span className="shrink-0 text-xs text-muted-foreground tabular-nums">
                  {d.durationSec}s
                </span>
              </div>
            ))
          )}
        </CardContent>
      </Card>
    </div>
  );
}
