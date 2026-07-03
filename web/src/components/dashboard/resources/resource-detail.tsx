"use client";

import * as React from "react";
import Link from "next/link";
import { useParams } from "next/navigation";
import { toast } from "sonner";
import {
  ArrowLeft,
  Rocket,
  RotateCw,
  ExternalLink,
  Server as ServerIcon,
  GitBranch,
  Globe,
  Tag,
  Boxes,
  Cpu,
  MemoryStick,
  Activity,
  Trash2,
  Power,
  ScrollText,
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
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Separator } from "@/components/ui/separator";
import { StatusBadge, StatusDot } from "@/components/dashboard/status-indicator";
import {
  getResource,
  getProject,
  getEnvironment,
  getServer,
  getMetrics,
  getLogs,
  getDeployments,
} from "@/lib/mock";
import {
  KIND_LABELS,
  SERVER_TYPE_LABELS,
  formatDateTime,
  formatDuration,
} from "./resource-meta";
import { MetricsChart } from "./metrics-chart";
import { LogsViewer } from "./logs-viewer";
import { EnvVarsTable } from "./env-vars-table";
import { DeployStatusBadge } from "./deploy-status-badge";
import { DeployWizard } from "./deploy-wizard";

function FactRow({
  icon: Icon,
  label,
  children,
}: {
  icon: React.ElementType;
  label: string;
  children: React.ReactNode;
}) {
  return (
    <div className="flex items-center justify-between gap-4 py-2.5">
      <span className="inline-flex items-center gap-2 text-sm text-muted-foreground">
        <Icon className="size-4" />
        {label}
      </span>
      <span className="min-w-0 truncate text-right text-sm text-foreground">
        {children}
      </span>
    </div>
  );
}

export function ResourceDetail() {
  const params = useParams<{ resourceId: string }>();
  const resourceId = params.resourceId;
  const [wizardOpen, setWizardOpen] = React.useState(false);

  const resource = React.useMemo(
    () => getResource(resourceId),
    [resourceId]
  );

  const project = resource ? getProject(resource.projectId) : undefined;
  const env = resource ? getEnvironment(resource.environmentId) : undefined;
  const server = resource ? getServer(resource.serverId) : undefined;

  const metrics = React.useMemo(
    () => getMetrics(resourceId),
    [resourceId]
  );
  const logs = React.useMemo(() => getLogs(resourceId), [resourceId]);
  const deployments = React.useMemo(
    () => getDeployments(resourceId),
    [resourceId]
  );

  if (!resource) {
    return (
      <div className="flex flex-col items-center justify-center gap-3 p-16 text-center">
        <Boxes className="size-8 text-muted-foreground" />
        <div className="flex flex-col gap-1">
          <p className="text-sm font-medium text-foreground">Resource not found</p>
          <p className="text-sm text-muted-foreground">
            It may have been removed or the link is stale.
          </p>
        </div>
        <Button
          variant="outline"
          size="sm"
          nativeButton={false}
          render={<Link href="/dashboard/resources" />}
        >
          <ArrowLeft className="size-4" />
          Back to resources
        </Button>
      </div>
    );
  }

  const latest = metrics[metrics.length - 1];

  return (
    <div className="flex flex-col gap-6 p-4 md:p-6">
      {/* Header */}
      <div className="flex flex-col gap-4">
        <Link
          href="/dashboard/resources"
          className="inline-flex w-fit items-center gap-1.5 text-sm text-muted-foreground hover:text-foreground"
        >
          <ArrowLeft className="size-4" />
          Resources
        </Link>

        <div className="flex flex-col gap-4 lg:flex-row lg:items-start lg:justify-between">
          <div className="flex flex-col gap-2">
            <div className="flex flex-wrap items-center gap-3">
              <h1 className="text-xl font-semibold tracking-tight text-foreground">
                {resource.name}
              </h1>
              <StatusBadge status={resource.status} />
              <Badge variant="outline" className="font-mono">
                {KIND_LABELS[resource.kind]}
              </Badge>
            </div>
            <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-sm text-muted-foreground">
              <span className="inline-flex items-center gap-1.5">
                <Boxes className="size-3.5" />
                {project?.name} / {env?.name}
              </span>
              {resource.domain && (
                <a
                  href={`https://${resource.domain}`}
                  onClick={(e) => e.preventDefault()}
                  className="inline-flex items-center gap-1.5 hover:text-foreground"
                >
                  <Globe className="size-3.5" />
                  {resource.domain}
                </a>
              )}
              {resource.version && (
                <span className="inline-flex items-center gap-1.5">
                  <Tag className="size-3.5" />
                  {resource.version}
                </span>
              )}
              {server && (
                <span className="inline-flex items-center gap-1.5">
                  <ServerIcon className="size-3.5" />
                  {server.name}
                </span>
              )}
            </div>
          </div>

          <div className="flex flex-wrap items-center gap-2">
            {resource.domain && (
              <Button
                variant="outline"
                size="sm"
                onClick={() => toast(`Opening https://${resource.domain}`)}
              >
                <ExternalLink className="size-4" />
                Open
              </Button>
            )}
            <Button
              variant="outline"
              size="sm"
              onClick={() => toast.success(`Restarting ${resource.name}…`)}
            >
              <RotateCw className="size-4" />
              Restart
            </Button>
            <Button size="sm" onClick={() => setWizardOpen(true)}>
              <Rocket className="size-4" />
              Deploy
            </Button>
          </div>
        </div>
      </div>

      {/* Tabs */}
      <Tabs defaultValue="overview">
        <TabsList variant="line" className="w-full justify-start overflow-x-auto">
          <TabsTrigger value="overview">Overview</TabsTrigger>
          <TabsTrigger value="logs">Logs</TabsTrigger>
          <TabsTrigger value="metrics">Metrics</TabsTrigger>
          <TabsTrigger value="environment">Environment</TabsTrigger>
          <TabsTrigger value="deployments">Deployments</TabsTrigger>
          <TabsTrigger value="settings">Settings</TabsTrigger>
        </TabsList>

        {/* Overview */}
        <TabsContent value="overview" className="pt-4">
          <div className="grid grid-cols-1 gap-4 lg:grid-cols-3">
            <Card className="lg:col-span-1">
              <CardHeader>
                <CardTitle>Details</CardTitle>
              </CardHeader>
              <CardContent className="flex flex-col divide-y divide-border py-0">
                <FactRow icon={Boxes} label="Kind">
                  {KIND_LABELS[resource.kind]}
                </FactRow>
                <FactRow icon={Activity} label="Status">
                  <span className="inline-flex items-center gap-1.5">
                    <StatusDot status={resource.status} />
                    {resource.status}
                  </span>
                </FactRow>
                <FactRow icon={GitBranch} label="Repository">
                  {resource.repo ? (
                    <span className="font-mono">{resource.repo}</span>
                  ) : (
                    <span className="text-muted-foreground">—</span>
                  )}
                </FactRow>
                <FactRow icon={Tag} label="Version">
                  {resource.version ?? "—"}
                </FactRow>
                <FactRow icon={Globe} label="Domain">
                  {resource.domain ?? (
                    <span className="text-muted-foreground">—</span>
                  )}
                </FactRow>
                <FactRow icon={ServerIcon} label="Server">
                  {server ? (
                    <span className="inline-flex items-center gap-1.5">
                      {server.name}
                      <Badge variant="outline" className="text-[10px]">
                        {SERVER_TYPE_LABELS[server.type]}
                      </Badge>
                    </span>
                  ) : (
                    "—"
                  )}
                </FactRow>
                <FactRow icon={Rocket} label="Last deploy">
                  {formatDateTime(resource.lastDeployAt)}
                </FactRow>
              </CardContent>
            </Card>

            <Card className="lg:col-span-2">
              <CardHeader className="border-b">
                <div className="flex items-center justify-between gap-4">
                  <div className="flex flex-col gap-1">
                    <CardTitle>Resource usage</CardTitle>
                    <CardDescription>Last 24 hours</CardDescription>
                  </div>
                  {latest && (
                    <div className="flex items-center gap-4 text-sm">
                      <span className="inline-flex items-center gap-1.5">
                        <Cpu className="size-4 text-muted-foreground" />
                        <span className="tabular-nums text-foreground">
                          {latest.cpu}%
                        </span>
                      </span>
                      <span className="inline-flex items-center gap-1.5">
                        <MemoryStick className="size-4 text-muted-foreground" />
                        <span className="tabular-nums text-foreground">
                          {latest.mem}%
                        </span>
                      </span>
                    </div>
                  )}
                </div>
              </CardHeader>
              <CardContent>
                <MetricsChart data={metrics} className="aspect-[16/7] w-full" />
              </CardContent>
            </Card>
          </div>
        </TabsContent>

        {/* Logs */}
        <TabsContent value="logs" className="pt-4">
          <Card>
            <CardHeader className="border-b">
              <div className="flex items-center justify-between gap-4">
                <div className="flex flex-col gap-1">
                  <CardTitle>Logs</CardTitle>
                  <CardDescription>
                    Live tail from {resource.name}
                  </CardDescription>
                </div>
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => toast("Refreshed log tail")}
                >
                  <ScrollText className="size-4" />
                  Refresh
                </Button>
              </div>
            </CardHeader>
            <CardContent>
              <LogsViewer logs={logs} />
            </CardContent>
          </Card>
        </TabsContent>

        {/* Metrics */}
        <TabsContent value="metrics" className="pt-4">
          <div className="grid grid-cols-1 gap-4">
            <Card>
              <CardHeader className="border-b">
                <CardTitle>CPU &amp; Memory</CardTitle>
                <CardDescription>Utilization %, last 24 hours</CardDescription>
              </CardHeader>
              <CardContent>
                <MetricsChart
                  data={metrics}
                  keys={["cpu", "mem"]}
                  className="aspect-[16/6] w-full"
                />
              </CardContent>
            </Card>
            <Card>
              <CardHeader className="border-b">
                <CardTitle>Network</CardTitle>
                <CardDescription>Throughput Mb/s, last 24 hours</CardDescription>
              </CardHeader>
              <CardContent>
                <MetricsChart
                  data={metrics}
                  keys={["net"]}
                  className="aspect-[16/6] w-full"
                />
              </CardContent>
            </Card>
          </div>
        </TabsContent>

        {/* Environment */}
        <TabsContent value="environment" className="pt-4">
          <Card>
            <CardHeader className="border-b">
              <CardTitle>Environment &amp; secrets</CardTitle>
              <CardDescription>
                Values are masked by default. Revealing a secret requires
                confirmation.
              </CardDescription>
            </CardHeader>
            <CardContent>
              <EnvVarsTable seedKey={resource.id} />
            </CardContent>
          </Card>
        </TabsContent>

        {/* Deployments */}
        <TabsContent value="deployments" className="pt-4">
          <Card>
            <CardHeader className="border-b">
              <CardTitle>Deployments</CardTitle>
              <CardDescription>Recent builds for {resource.name}</CardDescription>
            </CardHeader>
            <CardContent className="px-0">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead className="pl-4">Commit</TableHead>
                    <TableHead>Status</TableHead>
                    <TableHead>Author</TableHead>
                    <TableHead>Duration</TableHead>
                    <TableHead className="pr-4">Started</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {deployments.map((d) => (
                    <TableRow key={d.id}>
                      <TableCell className="pl-4">
                        <span className="font-mono text-sm text-foreground">
                          {d.sha}
                        </span>
                      </TableCell>
                      <TableCell>
                        <DeployStatusBadge status={d.status} />
                      </TableCell>
                      <TableCell className="text-muted-foreground">
                        {d.author}
                      </TableCell>
                      <TableCell className="tabular-nums text-muted-foreground">
                        {formatDuration(d.durationSec)}
                      </TableCell>
                      <TableCell className="pr-4 tabular-nums text-muted-foreground">
                        {formatDateTime(d.startedAt)}
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </CardContent>
          </Card>
        </TabsContent>

        {/* Settings */}
        <TabsContent value="settings" className="pt-4">
          <div className="flex flex-col gap-4">
            <Card>
              <CardHeader className="border-b">
                <CardTitle>General</CardTitle>
                <CardDescription>
                  Basic configuration for this resource.
                </CardDescription>
              </CardHeader>
              <CardContent className="flex flex-col divide-y divide-border py-0">
                <FactRow icon={Boxes} label="Name">
                  <span className="font-mono">{resource.name}</span>
                </FactRow>
                <FactRow icon={GitBranch} label="Auto-deploy on push">
                  <Badge variant="secondary">Enabled</Badge>
                </FactRow>
                <FactRow icon={Activity} label="Health checks">
                  <Badge variant="secondary">Enabled</Badge>
                </FactRow>
              </CardContent>
            </Card>

            <Card className="ring-destructive/20">
              <CardHeader className="border-b">
                <CardTitle className="text-destructive">Danger zone</CardTitle>
                <CardDescription>
                  These actions are irreversible.
                </CardDescription>
              </CardHeader>
              <CardContent className="flex flex-col gap-4 pt-0">
                <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
                  <div>
                    <p className="text-sm font-medium text-foreground">
                      Stop resource
                    </p>
                    <p className="text-sm text-muted-foreground">
                      Take {resource.name} offline. It can be restarted later.
                    </p>
                  </div>
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={() => toast(`${resource.name} stopped`)}
                  >
                    <Power className="size-4" />
                    Stop
                  </Button>
                </div>
                <Separator />
                <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
                  <div>
                    <p className="text-sm font-medium text-foreground">
                      Delete resource
                    </p>
                    <p className="text-sm text-muted-foreground">
                      Permanently remove {resource.name} and its data.
                    </p>
                  </div>
                  <Button
                    variant="destructive"
                    size="sm"
                    onClick={() =>
                      toast.error(`This is a prototype — ${resource.name} was not deleted.`)
                    }
                  >
                    <Trash2 className="size-4" />
                    Delete
                  </Button>
                </div>
              </CardContent>
            </Card>
          </div>
        </TabsContent>
      </Tabs>

      <DeployWizard
        open={wizardOpen}
        onOpenChange={setWizardOpen}
        orgId={project?.orgId ?? "org_acme"}
      />
    </div>
  );
}
