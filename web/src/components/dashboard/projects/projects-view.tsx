"use client";

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
import type { Status } from "@/lib/mock";
import { NewProjectDialog } from "./new-project-dialog";
import { ProjectCardMenu } from "./project-card-menu";

export type ProjectCardData = {
  id: string;
  name: string;
  description: string;
  envCount: number;
  serverCount: number;
  resourceCount: number;
  statusCounts: Record<string, number>;
};

// Priority order so the most-severe status floats to the front of the chip row.
const STATUS_PRIORITY: Status[] = [
  "error",
  "stopped",
  "degraded",
  "provisioning",
  "running",
];

function StatusChips({ counts }: { counts: Record<string, number> }) {
  const entries = STATUS_PRIORITY.filter((s) => counts[s]).map((s) => ({
    status: s,
    count: counts[s],
  }));

  if (entries.length === 0) {
    return <span className="text-xs text-muted-foreground">No resources yet</span>;
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
          {status}
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

function ProjectCard({ data }: { data: ProjectCardData }) {
  return (
    <Card className="group/project relative transition-colors hover:ring-foreground/20">
      <div className="absolute right-3 top-3">
        <ProjectCardMenu
          projectId={data.id}
          name={data.name}
          description={data.description}
        />
      </div>
      <CardHeader>
        <CardTitle className="pr-8">
          <Link
            href={`/dashboard/projects/${data.id}`}
            className="after:absolute after:inset-0 hover:underline"
          >
            {data.name}
          </Link>
        </CardTitle>
        <CardDescription className="line-clamp-2">
          {data.description || "No description"}
        </CardDescription>
      </CardHeader>
      <CardContent className="flex flex-wrap items-center gap-x-4 gap-y-1.5 text-xs text-muted-foreground">
        <MetaStat icon={Layers} value={data.envCount} label="envs" />
        <MetaStat icon={ServerIcon} value={data.serverCount} label="servers" />
        <MetaStat icon={Boxes} value={data.resourceCount} label="resources" />
      </CardContent>
      <CardFooter className="relative">
        <StatusChips counts={data.statusCounts} />
      </CardFooter>
    </Card>
  );
}

function EmptyState({ orgId }: { orgId: string }) {
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
        <NewProjectDialog orgId={orgId} />
      </CardContent>
    </Card>
  );
}

export function ProjectsView({
  orgId,
  orgName,
  projects,
}: {
  orgId: string;
  orgName: string;
  projects: ProjectCardData[];
}) {
  return (
    <div className="flex flex-col gap-6 p-4 md:p-6">
      <div className="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
        <div className="flex flex-col gap-1">
          <h1 className="text-xl font-semibold tracking-tight text-foreground">
            Projects
          </h1>
          <p className="text-sm text-muted-foreground">
            Projects group environments and resources across {orgName}.
          </p>
        </div>
        {projects.length > 0 && <NewProjectDialog orgId={orgId} />}
      </div>

      {projects.length === 0 ? (
        <EmptyState orgId={orgId} />
      ) : (
        <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
          {projects.map((p) => (
            <ProjectCard key={p.id} data={p} />
          ))}
        </div>
      )}
    </div>
  );
}
