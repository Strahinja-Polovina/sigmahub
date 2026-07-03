"use client";

import * as React from "react";
import Link from "next/link";
import { Boxes, FolderGit2, Layers, Server as ServerIcon } from "lucide-react";

import {
  Card,
  CardContent,
  CardDescription,
  CardFooter,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { StatusDot } from "@/components/dashboard/status-indicator";
import { useActiveOrg } from "@/components/dashboard/org-context";
import {
  getEnvironments,
  getProjects,
  getResourcesByProject,
} from "@/lib/mock";
import type { Project, Status } from "@/lib/mock";
import { NewProjectDialog } from "./new-project-dialog";

// Priority order so the most-severe status floats to the front of the chip row.
const STATUS_PRIORITY: Status[] = [
  "error",
  "stopped",
  "degraded",
  "provisioning",
  "running",
];

const STATUS_LABELS: Record<Status, string> = {
  running: "running",
  degraded: "degraded",
  provisioning: "provisioning",
  stopped: "stopped",
  error: "error",
};

type ProjectSummary = {
  project: Project;
  envCount: number;
  serverCount: number;
  resourceCount: number;
  statusCounts: Partial<Record<Status, number>>;
};

function summarize(project: Project): ProjectSummary {
  const envs = getEnvironments(project.id);
  const serverIds = new Set<string>();
  for (const env of envs) for (const id of env.serverIds) serverIds.add(id);

  const resources = getResourcesByProject(project.id);
  const statusCounts: Partial<Record<Status, number>> = {};
  for (const r of resources) {
    statusCounts[r.status] = (statusCounts[r.status] ?? 0) + 1;
  }

  return {
    project,
    envCount: envs.length,
    serverCount: serverIds.size,
    resourceCount: resources.length,
    statusCounts,
  };
}

function StatusChips({
  counts,
}: {
  counts: Partial<Record<Status, number>>;
}) {
  const entries = STATUS_PRIORITY.filter((s) => counts[s]).map((s) => ({
    status: s,
    count: counts[s] as number,
  }));

  if (entries.length === 0) {
    return (
      <span className="text-xs text-muted-foreground">No resources yet</span>
    );
  }

  return (
    <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
      {entries.map(({ status, count }) => (
        <span
          key={status}
          className="inline-flex items-center gap-1.5 text-xs text-muted-foreground"
        >
          <StatusDot status={status} />
          <span className="tabular-nums text-foreground">{count}</span>
          {STATUS_LABELS[status]}
        </span>
      ))}
    </div>
  );
}

function MetaStat({
  icon: Icon,
  value,
  label,
}: {
  icon: React.ElementType;
  value: number;
  label: string;
}) {
  return (
    <span className="inline-flex items-center gap-1.5">
      <Icon className="size-3.5 text-muted-foreground" />
      <span className="tabular-nums text-foreground">{value}</span>
      <span>{label}</span>
    </span>
  );
}

function ProjectCard({ summary }: { summary: ProjectSummary }) {
  const { project, envCount, serverCount, resourceCount, statusCounts } =
    summary;
  return (
    <Card className="group/project relative transition-colors hover:ring-foreground/20">
      <CardHeader>
        <CardTitle>
          <Link
            href={`/dashboard/projects/${project.id}`}
            className="after:absolute after:inset-0 hover:underline"
          >
            {project.name}
          </Link>
        </CardTitle>
        <CardDescription className="line-clamp-2">
          {project.description}
        </CardDescription>
      </CardHeader>
      <CardContent className="flex flex-wrap items-center gap-x-4 gap-y-1.5 text-xs text-muted-foreground">
        <MetaStat icon={Layers} value={envCount} label="envs" />
        <MetaStat icon={ServerIcon} value={serverCount} label="servers" />
        <MetaStat icon={Boxes} value={resourceCount} label="resources" />
      </CardContent>
      <CardFooter className="relative">
        <StatusChips counts={statusCounts} />
      </CardFooter>
    </Card>
  );
}

function EmptyState() {
  return (
    <Card>
      <CardContent className="flex flex-col items-center gap-3 py-12 text-center">
        <span className="grid size-10 place-items-center rounded-lg bg-muted text-muted-foreground">
          <FolderGit2 className="size-5" />
        </span>
        <div className="flex flex-col gap-1">
          <p className="text-sm font-medium text-foreground">No projects yet</p>
          <p className="text-sm text-muted-foreground">
            Create your first project to organize environments and resources.
          </p>
        </div>
        <NewProjectDialog />
      </CardContent>
    </Card>
  );
}

export function ProjectsView() {
  const { orgId, org } = useActiveOrg();

  const summaries = React.useMemo(
    () => getProjects(orgId).map(summarize),
    [orgId]
  );

  return (
    <div className="flex flex-col gap-6 p-4 md:p-6">
      <div className="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
        <div className="flex flex-col gap-1">
          <h1 className="text-xl font-semibold tracking-tight text-foreground">
            Projects
          </h1>
          <p className="text-sm text-muted-foreground">
            Projects group environments and resources across {org.name}.
          </p>
        </div>
        {summaries.length > 0 && <NewProjectDialog />}
      </div>

      {summaries.length === 0 ? (
        <EmptyState />
      ) : (
        <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
          {summaries.map((summary) => (
            <ProjectCard key={summary.project.id} summary={summary} />
          ))}
        </div>
      )}
    </div>
  );
}
