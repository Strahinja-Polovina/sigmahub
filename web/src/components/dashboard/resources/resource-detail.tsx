"use client";

import * as React from "react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { toast } from "sonner";
import {
  ArrowLeft,
  Rocket,
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
  ScrollText,
  Loader2,
  CircleAlert,
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
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Separator } from "@/components/ui/separator";
import { StatusBadge, StatusDot } from "@/components/dashboard/status-indicator";
import { getMetrics, getLogs } from "@/lib/sample-telemetry";
import { isDatabaseEngine } from "@/lib/server-catalog.generated";
import type { ResourceKind, ServerType, Status } from "@/lib/mock";
import {
  advanceDeployment,
  deleteResource,
  deployResource,
} from "@/server/actions/resources";
import {
  DEPLOY_STATUS_META,
  KIND_LABELS,
  SERVER_TYPE_LABELS,
  formatDateTime,
  formatDuration,
  isDeployInFlight,
} from "./resource-meta";
import { MetricsChart } from "./metrics-chart";
import { LogsViewer } from "./logs-viewer";
import { VolumeDeleteDialog } from "./volume-delete-dialog";
import { EnvVarsTable } from "./env-vars-table";
import { DeployStatusBadge } from "./deploy-status-badge";
import { ResourceDomainsPanel, type DomainRow } from "./resource-domains-panel";
import { DeploymentsPanel, type DeploymentRow } from "./deployments-panel";
import { DatabasePanel } from "./database-panel";
import { S3Panel } from "./s3-panel";
import { ComposeServicesPanel, type PlacementServer } from "./compose-services-panel";
import { DatabaseBackupsPanel } from "./database-backups-panel";
import { ControlPlaneNote } from "@/components/dashboard/control-plane-note";
import type {
  CpHealthCheck,
  CpDatabaseInfo,
  CpS3Info,
  CpComposeServices,
  CpBackupTarget,
  CpBackupRun,
  CpTelemetryPoint,
  CpLogLine,
} from "@/server/cp";

/** CP-mode telemetry payload. `pipeline` false = VictoriaMetrics/Loki are not
 *  configured — the UI shows an explicit state, never fabricated series.
 *
 *  There are THREE states here, not two, and the page must be able to tell them
 *  apart: not configured, configured-but-empty, and could-not-ask. The loader
 *  used to collapse the third into the second — a failed read answered
 *  `pipeline: true` with empty series, which is the page asserting the pipeline
 *  is configured and the container produced no output (SIGMA-236). */
export type CpTelemetry = {
  pipeline: boolean;
  /** The control plane did not answer. Not "there is nothing", but "we could
   *  not ask" — the distinction an operator acts on when a container is
   *  crash-looping. */
  unreadable?: boolean;
  metrics: CpTelemetryPoint[];
  logs: CpLogLine[];
};

type Deployment = {
  id: string;
  sha: string;
  status: string;
  author: string;
  durationSec: number;
  startedAt: string | Date;
};

type Detail = {
  resource: {
    id: string;
    name: string;
    kind: string;
    status: string;
    repo: string | null;
    domain: string | null;
    version: string | null;
    lastDeployAt: string | Date;
    serverId: string | null;
  };
  projectName: string;
  envName: string;
  server: { id: string; name: string; type: string } | null;
  /** The other kind of deploy target. Exactly one of server and cluster is set;
   *  a workload in a cluster has no server because the scheduler picks its
   *  node, and rendering "—" for it said the resource ran nowhere. */
  cluster: { id: string; name: string } | null;
  deployments: Deployment[];
  secrets: { id: string; name: string; envVar: boolean; scope: "project" | "environment" }[];
  canManage: boolean;
};

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

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
      <span className="min-w-0 truncate text-right text-sm text-foreground">{children}</span>
    </div>
  );
}

function RedeployButton({ resourceId }: { resourceId: string }) {
  const router = useRouter();
  const [busy, setBusy] = React.useState(false);

  async function redeploy() {
    setBusy(true);
    try {
      const res = await deployResource({ resourceId });
      if ("error" in res) {
        toast.error("Deploy failed", { description: res.error });
        return;
      }
      router.refresh();
      // CP mode: the control plane drives the real clone→build→rollout pipeline;
      // its progress shows live in the Deployments tab, so there's nothing to
      // simulate here.
      if ("cp" in res && res.cp) {
        if (res.reapplied) {
          toast.info("Re-apply queued", {
            description:
              "No build pipeline for this resource — the agent re-runs its setup with the last known config.",
          });
        } else {
          toast.info("Deployment queued", {
            description: "Building & rolling out — watch the Deployments tab.",
          });
        }
        return;
      }
      toast.info("Deployment queued", { description: "Building the image…" });
      await sleep(1100);
      await advanceDeployment({ deploymentId: res.deploymentId }); // → building
      router.refresh();
      await sleep(1100);
      await advanceDeployment({ deploymentId: res.deploymentId }); // → running
      router.refresh();
      toast.success("Deployed", { description: "Health checks passed · now serving traffic." });
    } catch (err) {
      toast.error("Deploy failed", {
        description: err instanceof Error ? err.message : "Please try again.",
      });
    } finally {
      setBusy(false);
    }
  }

  return (
    <Button size="sm" onClick={redeploy} disabled={busy}>
      {busy ? <Loader2 className="size-4 animate-spin" /> : <Rocket className="size-4" />}
      Deploy
    </Button>
  );
}

/** Delete is confirm-gated and typed: it cascades the resource's entire
 *  deployment history — every sibling destructive action on this page already
 *  confirms, this one used to fire straight from onClick (SIGMA-185).
 *
 *  A database's restic repo key is no longer destroyed with it: the control
 *  plane archives the wrapped key before the cascade (SIGMA-170), so the
 *  snapshots left in the customer's bucket stay openable. */
function DeleteResourceButton({
  resourceId,
  name,
  kind,
  ephemeral = false,
}: {
  resourceId: string;
  name: string;
  kind: string;
  /** PR-preview resource (SIGMA-194): deleting it permanently breaks the open
   *  preview for its PR — the dialog says so instead of presenting it as an
   *  ordinary resource. */
  ephemeral?: boolean;
}) {
  const router = useRouter();
  const [pending, startTransition] = React.useTransition();
  const [typed, setTyped] = React.useState("");
  // Which sentence this dialog owes the operator turns on whether the resource
  // is a managed engine — its volumes and its restic repo key survive the
  // cascade, an app's volumes merely stay behind (SIGMA-170/185). That is the
  // control plane's engine table, so it is asked rather than restated: the four
  // kinds used to be typed out here, and a fifth engine added to the Go catalog
  // would have been deleted under the wrong warning, telling its owner nothing
  // about the snapshots still sitting in their bucket (SIGMA-216).
  const isDatabase = isDatabaseEngine(kind);

  function confirmDelete() {
    startTransition(async () => {
      try {
        await deleteResource({ resourceId });
        toast.success(`${name} deleted`);
        router.push("/dashboard/resources");
      } catch (err) {
        toast.error("Couldn’t delete", {
          description: err instanceof Error ? err.message : "Please try again.",
        });
      }
    });
  }

  return (
    <Dialog onOpenChange={(open) => !open && setTyped("")}>
      <DialogTrigger
        render={
          <Button variant="destructive" size="sm" disabled={pending}>
            <Trash2 className="size-4" />
            Delete
          </Button>
        }
      />
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Delete {name}?</DialogTitle>
          <DialogDescription>
            {ephemeral
              ? "This is a PR preview resource. Deleting it permanently breaks the open preview for its pull request — the preview will not come back on the next push. To tear it down cleanly, close the PR instead."
              : "This removes the resource and its entire deployment history."}
            {!ephemeral &&
              (isDatabase
                ? " Data volumes and any snapshots already in your bucket are left in place, and the backup encryption key is retained so those snapshots can still be restored."
                : " Data volumes are left in place and must be removed separately.")}
          </DialogDescription>
        </DialogHeader>
        <div className="flex flex-col gap-1.5">
          <Label htmlFor="confirm-resource-name" className="text-xs text-muted-foreground">
            Type <span className="font-mono text-foreground">{name}</span> to confirm
          </Label>
          <Input
            id="confirm-resource-name"
            value={typed}
            onChange={(e) => setTyped(e.target.value)}
            autoComplete="off"
          />
        </div>
        <DialogFooter>
          <DialogClose render={<Button variant="outline" type="button" disabled={pending} />}>
            Cancel
          </DialogClose>
          <Button
            variant="destructive"
            type="button"
            disabled={pending || typed.trim() !== name}
            onClick={confirmDelete}
          >
            {pending && <Loader2 className="size-4 animate-spin" />}
            Delete resource
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

/** The branch mapping that decides whether a push deploys this resource's
 *  environment. `null` means the control plane has no mapping for it. */
export type AutoDeployPolicy = { branch: string; policy: "auto" | "manual" };

/** One line describing the probe the control plane actually runs, e.g.
 *  "HTTP GET /healthz on :3000" or "TCP probe on :3000". Returns null when
 *  there is no probe to describe — the caller renders "None", which is a
 *  different statement from a badge reading "Enabled". */
function describeHealthCheck(hc: CpHealthCheck | null | undefined): string | null {
  if (!hc || !hc.type) return null;
  const on = hc.port ? ` on :${hc.port}` : "";
  if (hc.type === "http") return `HTTP GET ${hc.path || "/"}${on}`;
  if (hc.type === "tcp") return `TCP probe${on}`;
  return `${hc.type}${on}`;
}

/** "a, b and c" — the failed reads read as prose rather than a bare array. */
function formatList(items: string[]): string {
  if (items.length === 1) return items[0];
  return items.slice(0, -1).join(", ") + " and " + items[items.length - 1];
}

export function ResourceDetail({
  detail,
  orgId,
  domains = [],
  domainsEnabled = false,
  cpDeployments,
  rollbackTargetIds = [],
  deploymentsEnabled = false,
  database = null,
  s3 = null,
  compose = null,
  placementServers = [],
  backupTargets = [],
  backupRuns = [],
  environmentId = "",
  cpTelemetry = null,
  statusError = null,
  loadFailures = [],
  autoDeploy = null,
  healthCheck = null,
}: {
  detail: Detail;
  orgId?: string;
  /** The live per-resource failure the agent reported (mesh bind, image pull,
   *  health-check timeout…). Rendered as a banner so an errored resource
   *  explains itself. */
  statusError?: string | null;
  /** Control-plane reads that failed while building this page. An empty panel
   *  and an unreadable one look identical, and the user acts on the difference:
   *  "no domains" means none are attached, not that we could not ask. */
  loadFailures?: string[];
  domains?: DomainRow[];
  /** True only when the control plane backs custom domains; the panel is hidden
   *  in demo mode where the attach/detach actions would error. */
  domainsEnabled?: boolean;
  /** CP-backed release history (P1-9). Present only when deploymentsEnabled. */
  cpDeployments?: DeploymentRow[];
  /** Deployment ids that are rebuild-free rollback candidates. */
  rollbackTargetIds?: string[];
  /** True when the control plane backs deployments; swaps the demo timeline for
   *  the real release history + build logs + rollback. */
  deploymentsEnabled?: boolean;
  /** P1-10 database connection metadata (CP mode, database kinds only). */
  database?: CpDatabaseInfo | null;
  /** P2-1 S3 endpoint metadata (CP mode, s3 kind only). */
  s3?: CpS3Info | null;
  /** Compose service graph + placement (CP mode, multi-service apps only). */
  compose?: CpComposeServices | null;
  /** Servers a compose service may be placed on. */
  placementServers?: PlacementServer[];
  /** P1-11 backups (CP mode, database kinds only). */
  backupTargets?: CpBackupTarget[];
  backupRuns?: CpBackupRun[];
  environmentId?: string;
  /** P1-13 telemetry (CP mode): real pipeline data or the explicit
   *  not-configured state. null = demo mode (labeled synthetic data). */
  cpTelemetry?: CpTelemetry | null;
  /** The branch→environment mapping governing this resource's deploys, from the
   *  control plane's git connection. null = no mapping (or nothing to ask). */
  autoDeploy?: AutoDeployPolicy | null;
  /** The health probe stored on the resource's spec. null = none. */
  healthCheck?: CpHealthCheck | null;
}) {
  const { resource, projectName, envName, server, cluster, deployments, secrets, canManage } =
    detail;
  const router = useRouter();

  // A deployment the control plane is still working on. Everything this page
  // says about "now" — the banner, the logs, the deployment list — is a server
  // render, so while one of these exists the page has to go back and ask.
  const inFlight = deployments.find((d) => isDeployInFlight(d.status)) ?? null;

  // Poll while a deploy is in flight. This page had no refresh path at all: the
  // clone→build→rollout pipeline can run for minutes, and the only way to see
  // that it had moved was a full navigation. Five seconds is well under the
  // shortest phase and the refresh is a server re-render of data the page
  // already fetches, not a new endpoint.
  //
  // The dependency is the BOOLEAN, not the deployment: `inFlight` is a fresh
  // object every render, and router.refresh() causes a render, so depending on
  // it would tear down and re-arm the interval before it ever fired.
  const deployInFlight = inFlight !== null;
  React.useEffect(() => {
    if (!deployInFlight) return;
    const id = setInterval(() => router.refresh(), 5000);
    return () => clearInterval(id);
  }, [deployInFlight, router]);

  const showDomains = Boolean(domainsEnabled && orgId && resource.kind === "app");
  const showCpDeployments = Boolean(deploymentsEnabled && orgId);

  // CP mode renders REAL pipeline telemetry (or the explicit not-configured
  // state); only demo mode falls back to the labeled synthetic series.
  const isCp = cpTelemetry !== null;
  const demoMetrics = React.useMemo(
    () => (isCp ? [] : getMetrics(resource.id)),
    [isCp, resource.id]
  );
  const demoLogs = React.useMemo(
    () => (isCp ? [] : getLogs(resource.id)),
    [isCp, resource.id]
  );
  const metrics = isCp ? cpTelemetry.metrics : demoMetrics;
  const logs = isCp ? cpTelemetry.logs : demoLogs;
  const pipelineOff = isCp && !cpTelemetry.pipeline;
  const telemetryUnreadable = isCp && Boolean(cpTelemetry.unreadable);
  const latest = metrics[metrics.length - 1];

  // Three states, three sentences. Saying "no telemetry received yet" when the
  // read FAILED sends an operator whose container is crash-looping off to hunt
  // on the host for output that was in fact never asked for (SIGMA-236).
  const telemetryEmptyState = telemetryUnreadable ? (
    <p className="py-8 text-center text-sm text-muted-foreground">
      Couldn’t read logs and metrics — the control plane didn’t answer. This is
      not the same as “nothing was produced”: reload once it is reachable.
    </p>
  ) : pipelineOff ? (
    <p className="py-8 text-center text-sm text-muted-foreground">
      Telemetry pipeline not configured — set CP_VM_WRITE_URL / CP_VM_READ_URL /
      CP_LOKI_URL on the control plane to collect metrics and logs.
    </p>
  ) : (
    <p className="py-8 text-center text-sm text-muted-foreground">
      No telemetry received yet — data appears within a minute of the agent’s
      first ship.
    </p>
  );

  return (
    <div className="flex flex-col gap-6 p-4 md:p-6">
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
              <StatusBadge status={resource.status as Status} />
              <Badge variant="outline" className="font-mono">
                {KIND_LABELS[resource.kind as ResourceKind]}
              </Badge>
              {"ephemeral" in resource && Boolean(resource.ephemeral) && (
                <Badge variant="secondary" className="text-[10px]">
                  PR preview
                </Badge>
              )}
            </div>
            <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-sm text-muted-foreground">
              <span className="inline-flex items-center gap-1.5">
                <Boxes className="size-3.5" />
                {projectName} / {envName}
              </span>
              {/* A real link. This was an <a href> whose onClick called
                  preventDefault, so clicking the hostname did nothing at all —
                  it read as broken rather than as decoration. In CP mode the
                  domain is a real hostname with a real Let's Encrypt
                  certificate the proxy issued, so there is somewhere to go
                  (SIGMA-238). */}
              {resource.domain && (
                <a
                  href={`https://${resource.domain}`}
                  target="_blank"
                  rel="noopener noreferrer"
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
              {cluster && (
                <span className="inline-flex items-center gap-1.5">
                  <Boxes className="size-3.5" />
                  {cluster.name}
                </span>
              )}
            </div>
          </div>

          <div className="flex flex-wrap items-center gap-2">
            {/* Likewise an anchor, not a toast. `toast("Opening https://…")`
                is the single most-hit dead control in the product: it is the
                very next click after attaching a domain and watching its
                certificate go green (SIGMA-238). */}
            {resource.domain && (
              <Button
                variant="outline"
                size="sm"
                render={
                  <a
                    href={`https://${resource.domain}`}
                    target="_blank"
                    rel="noopener noreferrer"
                  />
                }
              >
                <ExternalLink className="size-4" />
                Open
              </Button>
            )}
            <RedeployButton resourceId={resource.id} />
          </div>
        </div>
      </div>

      {loadFailures.length > 0 && (
        <div className="flex items-start gap-3 rounded-lg border border-amber-500/30 bg-amber-500/5 p-3">
          <CircleAlert className="mt-0.5 size-4 shrink-0 text-amber-600 dark:text-amber-500" />
          <div className="min-w-0 text-sm">
            <p className="font-medium text-amber-700 dark:text-amber-500">
              Some of this page couldn&apos;t be loaded
            </p>
            <p className="mt-0.5 text-xs text-muted-foreground">
              The control plane didn&apos;t answer for {formatList(loadFailures)}. What you see
              below is incomplete — an empty section here does not mean there is nothing
              there. Reload once the control plane is reachable.
            </p>
          </div>
        </div>
      )}

      {/* An in-flight deploy outranks the last one's error. statusError is the
          last APPLIED state, so it keeps describing a failure the running
          pipeline may already be fixing — and it told the user to "press
          Deploy" while their deploy was three minutes into its build. */}
      {inFlight ? (
        <div className="flex items-start gap-3 rounded-lg border border-border bg-muted/40 p-3">
          <Rocket className="mt-0.5 size-4 shrink-0 text-muted-foreground" />
          <div className="min-w-0 text-sm">
            <p className="font-medium">Deployment in progress</p>
            <p className="mt-0.5 text-xs text-muted-foreground">
              {DEPLOY_STATUS_META[inFlight.status]?.label ?? inFlight.status} — this
              page refreshes itself until it finishes. Build output is in the
              Deployments tab; the Logs tab keeps showing the version that is
              serving traffic until the new one passes its health check.
            </p>
            {statusError && (
              <p className="mt-1 text-xs text-muted-foreground">
                Previous failure: <span className="font-mono">{statusError}</span>
              </p>
            )}
          </div>
        </div>
      ) : (
        statusError && (
          <div className="flex items-start gap-3 rounded-lg border border-destructive/30 bg-destructive/5 p-3">
            <CircleAlert className="mt-0.5 size-4 shrink-0 text-destructive" />
            <div className="min-w-0 text-sm">
              <p className="font-medium text-destructive">This resource is failing</p>
              <p className="mt-0.5 break-words font-mono text-xs text-destructive/90">
                {statusError}
              </p>
              <p className="mt-1 text-xs text-muted-foreground">
                Fix the cause, then press Deploy to re-apply. Logs and metrics
                stay empty until the container starts.
              </p>
            </div>
          </div>
        )
      )}

      <Tabs defaultValue="overview">
        <TabsList variant="line" className="w-full justify-start overflow-x-auto">
          <TabsTrigger value="overview">Overview</TabsTrigger>
          <TabsTrigger value="logs">Logs</TabsTrigger>
          <TabsTrigger value="metrics">Metrics</TabsTrigger>
          <TabsTrigger value="environment">Environment</TabsTrigger>
          <TabsTrigger value="deployments">Deployments</TabsTrigger>
          <TabsTrigger value="settings">Settings</TabsTrigger>
        </TabsList>

        <TabsContent value="overview" className="pt-4">
          <div className="grid grid-cols-1 gap-4 lg:grid-cols-3">
            <Card className="lg:col-span-1">
              <CardHeader>
                <CardTitle>Details</CardTitle>
              </CardHeader>
              <CardContent className="flex flex-col divide-y divide-border py-0">
                <FactRow icon={Boxes} label="Kind">
                  {KIND_LABELS[resource.kind as ResourceKind]}
                </FactRow>
                <FactRow icon={Activity} label="Status">
                  <span className="inline-flex items-center gap-1.5">
                    <StatusDot status={resource.status as Status} />
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
                  {resource.domain ?? <span className="text-muted-foreground">—</span>}
                </FactRow>
                {/* One row for whichever target this resource has. Naming it
                    "Server" and printing "—" for a cluster workload was the
                    page saying it ran nowhere. */}
                <FactRow
                  icon={cluster ? Boxes : ServerIcon}
                  label={cluster ? "Cluster" : "Server"}
                >
                  {cluster ? (
                    <span className="inline-flex items-center gap-1.5">
                      {cluster.name}
                      <Badge variant="outline" className="text-[10px]">
                        Kubernetes
                      </Badge>
                    </span>
                  ) : server ? (
                    <span className="inline-flex items-center gap-1.5">
                      {server.name}
                      <Badge variant="outline" className="text-[10px]">
                        {SERVER_TYPE_LABELS[server.type as ServerType]}
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

            <div className="flex flex-col gap-4 lg:col-span-2">
              {compose && compose.services.length > 0 && orgId && (
                <ComposeServicesPanel
                  orgId={orgId}
                  resourceId={resource.id}
                  services={compose.services}
                  homeServerId={compose.homeServerId}
                  servers={placementServers}
                  canManage={canManage}
                />
              )}
              {database && orgId && (
                <DatabasePanel
                  orgId={orgId}
                  resourceId={resource.id}
                  info={database}
                  canManage={canManage}
                  simulated={!isCp}
                />
              )}
              {s3 && orgId && (
                <S3Panel
                  orgId={orgId}
                  resourceId={resource.id}
                  info={s3}
                  canManage={canManage}
                  simulated={!isCp}
                />
              )}
              {/* Backups are the control plane running restic against a real S3
                  target on a schedule. There is nothing offline to schedule,
                  verify or restore, so the panel is replaced by what it would
                  have done rather than by nothing at all (SIGMA-215). */}
              {database && orgId && !isCp && (
                <ControlPlaneNote title="Backups run on the control plane">
                  With a control plane, this database is backed up on a schedule to an
                  S3-compatible target you own, every backup is verified by restoring it,
                  and PostgreSQL additionally streams WAL so you can recover to a
                  point in time. None of that can happen here: there is no engine to dump
                  and nowhere to write to.
                </ControlPlaneNote>
              )}
              {database && orgId && isCp && (
                <DatabaseBackupsPanel
                  orgId={orgId}
                  resourceId={resource.id}
                  environmentId={environmentId}
                  serverId={resource.serverId}
                  resourceName={resource.name}
                  policy={database.backupPolicy ?? null}
                  targets={backupTargets}
                  runs={backupRuns}
                  canManage={canManage}
                  engine={database.engine}
                  pitrWindow={{
                    lastWalAt: database.lastWalAt,
                    lastWalSegment: database.lastWalSegment,
                  }}
                />
              )}
              <Card>
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
                          <span className="tabular-nums text-foreground">{latest.cpu}%</span>
                        </span>
                        <span className="inline-flex items-center gap-1.5">
                          <MemoryStick className="size-4 text-muted-foreground" />
                          <span className="tabular-nums text-foreground">
                            {isCp ? `${latest.mem} MiB` : `${latest.mem}%`}
                          </span>
                        </span>
                      </div>
                    )}
                  </div>
                </CardHeader>
                <CardContent>
                  {isCp && metrics.length === 0 ? (
                    telemetryEmptyState
                  ) : (
                    <MetricsChart
                      data={metrics}
                      keys={isCp ? ["cpu", "mem"] : ["cpu", "mem", "net"]}
                      memUnit={isCp ? "MiB" : "%"}
                      className="aspect-[16/7] w-full"
                    />
                  )}
                </CardContent>
              </Card>
            </div>
          </div>
        </TabsContent>

        <TabsContent value="logs" className="pt-4">
          <Card>
            <CardHeader className="border-b">
              <div className="flex items-center justify-between gap-4">
                <div className="flex flex-col gap-1">
                  <CardTitle>Logs</CardTitle>
                  <CardDescription>
                    {/* Not "Live tail": this is a snapshot of the newest lines,
                        taken when the page rendered. Calling it live is what
                        made a stale tail look like a stuck container. */}
                    Newest output from {resource.name}
                    {inFlight ? " — refreshing while the deployment runs" : ""}
                  </CardDescription>
                </div>
                {/* This used to be `onClick={() => toast("Refreshed log tail")}`
                    — a button that reported success and fetched nothing, on a
                    card headed "Live tail". The tail is a server render, so
                    refreshing it is refreshing the route. */}
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => {
                    router.refresh();
                    toast("Refreshing log tail…");
                  }}
                >
                  <ScrollText className="size-4" />
                  Refresh
                </Button>
              </div>
            </CardHeader>
            <CardContent>
              {isCp && logs.length === 0 ? telemetryEmptyState : <LogsViewer logs={logs} />}
            </CardContent>
          </Card>
        </TabsContent>

        <TabsContent value="metrics" className="pt-4">
          <div className="grid grid-cols-1 gap-4">
            <Card>
              <CardHeader className="border-b">
                <CardTitle>CPU &amp; Memory</CardTitle>
                <CardDescription>
                  {isCp ? "CPU % and memory MiB, last 24 hours" : "Utilization %, last 24 hours"}
                </CardDescription>
              </CardHeader>
              <CardContent>
                {isCp && metrics.length === 0 ? (
                  telemetryEmptyState
                ) : (
                  <MetricsChart data={metrics} keys={["cpu", "mem"]} memUnit={isCp ? "MiB" : "%"} className="aspect-[16/6] w-full" />
                )}
              </CardContent>
            </Card>
            {!isCp && (
              <Card>
                <CardHeader className="border-b">
                  <CardTitle>Network</CardTitle>
                  <CardDescription>Throughput Mb/s, last 24 hours</CardDescription>
                </CardHeader>
                <CardContent>
                  <MetricsChart data={metrics} keys={["net"]} className="aspect-[16/6] w-full" />
                </CardContent>
              </Card>
            )}
          </div>
        </TabsContent>

        <TabsContent value="environment" className="pt-4">
          <Card>
            <CardHeader className="border-b">
              <CardTitle>Environment &amp; secrets</CardTitle>
              <CardDescription>
                Effective secrets for this resource — its environment plus shared project secrets.
                Values are masked; revealing one is audited.
              </CardDescription>
            </CardHeader>
            <CardContent>
              <EnvVarsTable
                resourceId={resource.id}
                envName={envName}
                secrets={secrets}
                canManage={canManage}
              />
            </CardContent>
          </Card>
        </TabsContent>

        <TabsContent value="deployments" className="pt-4">
          {showCpDeployments ? (
            <DeploymentsPanel
              orgId={orgId!}
              resourceId={resource.id}
              deployments={cpDeployments ?? []}
              rollbackTargetIds={rollbackTargetIds}
              canManage={canManage}
            />
          ) : (
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
                          <span className="font-mono text-sm text-foreground">{d.sha}</span>
                        </TableCell>
                        <TableCell>
                          <DeployStatusBadge status={d.status} />
                        </TableCell>
                        <TableCell className="text-muted-foreground">{d.author}</TableCell>
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
                {/* The demo timeline is real enough to walk — a redeploy queues
                    here and advances — but three things on this tab are the
                    control plane's pipeline and cannot be, so they are named
                    rather than silently missing (SIGMA-215). */}
                <div className="px-4 pt-4">
                  <ControlPlaneNote title="Build logs and rollback come from the pipeline">
                    With a control plane, each release here carries the clone, build and
                    rollout it came from, streams that build&apos;s logs while it runs, and
                    can be rolled back to any earlier successful release without rebuilding
                    — the image is already in your registry. That pipeline is the control
                    plane cloning your repository onto a build server, so there is nothing
                    offline to stream or roll back to.
                  </ControlPlaneNote>
                </div>
              </CardContent>
            </Card>
          )}
        </TabsContent>

        <TabsContent value="settings" className="pt-4">
          <div className="flex flex-col gap-4">
            <Card>
              <CardHeader className="border-b">
                <CardTitle>General</CardTitle>
                <CardDescription>Basic configuration for this resource.</CardDescription>
              </CardHeader>
              <CardContent className="flex flex-col divide-y divide-border py-0">
                <FactRow icon={Boxes} label="Name">
                  <span className="font-mono">{resource.name}</span>
                </FactRow>
                {/* Both of these used to be the literal
                    `<Badge variant="secondary">Enabled</Badge>` with no data
                    behind them. A user who mapped their branch as "Manual
                    promote" in the Git panel read "Auto-deploy on push:
                    Enabled" here, pushed, and waited for a rollout that was
                    never going to happen — and the project panel's "Recent
                    pushes" correctly saying the push deployed nothing then read
                    as the lagging one. The same badge claimed auto-deploy and
                    health checks for a database, which has no repository at all
                    (SIGMA-240).

                    Both now render what the control plane actually holds: the
                    branch mapping's policy, and the probe on the resource's
                    spec. */}
                <FactRow icon={GitBranch} label="Auto-deploy on push">
                  {!resource.repo ? (
                    <span className="text-muted-foreground">Not connected to a repository</span>
                  ) : autoDeploy ? (
                    autoDeploy.policy === "auto" ? (
                      <Badge variant="secondary">
                        On push to <span className="font-mono">{autoDeploy.branch}</span>
                      </Badge>
                    ) : (
                      <Badge variant="outline">Manual promote</Badge>
                    )
                  ) : (
                    <span className="text-muted-foreground">No branch mapped</span>
                  )}
                </FactRow>
                <FactRow icon={Activity} label="Health checks">
                  {describeHealthCheck(healthCheck) ?? (
                    <span className="text-muted-foreground">None</span>
                  )}
                </FactRow>
              </CardContent>
            </Card>

            {showDomains && (
              <ResourceDomainsPanel
                orgId={orgId!}
                resourceId={resource.id}
                domains={domains}
                canManage={canManage}
              />
            )}
            {/* Not hidden any more. A custom domain is a certificate issued by
                ACME and a route programmed into Traefik on an edge server —
                nothing a demo can do — but hiding the panel meant an evaluator
                concluded the product has no custom domains, rather than that
                they cannot watch one be issued here (SIGMA-215). */}
            {!showDomains && resource.kind === "app" && (
              <ControlPlaneNote title="Custom domains are issued by the control plane">
                With a control plane, you point a hostname at a server carrying the edge
                role and SigmaHub programmes the route, requests a Let&apos;s Encrypt
                certificate over HTTP-01 or DNS-01, renews it, and shows you the exact DNS
                records to create and whether they have propagated. Issuing a real
                certificate needs a real hostname and a reachable host, so there is
                nothing here to issue.
              </ControlPlaneNote>
            )}

            <Card className="ring-destructive/20">
              <CardHeader className="border-b">
                <CardTitle className="text-destructive">Danger zone</CardTitle>
                <CardDescription>These actions are irreversible.</CardDescription>
              </CardHeader>
              {/* "Stop resource" used to live here, and its entire handler was
                  `toast(`${resource.name} stopped`)` — a control in a card
                  headed "These actions are irreversible" that reported an
                  irreversible action it never performed. There is no CP stop
                  endpoint and no desired-state change behind it: the container
                  kept serving traffic, kept its database connections and kept
                  being billed, while the status badge (correctly) still read
                  Running, which an operator mid-incident reads as stale UI
                  rather than as the truth. Removed until a real scale-to-zero /
                  container.stop op exists that the reconciler renders and the
                  status badge reflects — the same treatment the Overview
                  dropdown's fake Deploy/Restart items already got (SIGMA-234,
                  SIGMA-162). */}
              <CardContent className="flex flex-col gap-4 pt-4">
                {resource.kind === "app" && resource.serverId && (
                  <>
                    <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
                      <div>
                        <p className="text-sm font-medium text-foreground">Delete a data volume</p>
                        <p className="text-sm text-muted-foreground">
                          Permanently destroy a named volume’s data. Requires a confirmation token and approval.
                        </p>
                      </div>
                      <VolumeDeleteDialog resourceId={resource.id} resourceName={resource.name} />
                    </div>
                    <Separator />
                  </>
                )}
                <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
                  <div>
                    <p className="text-sm font-medium text-foreground">Delete resource</p>
                    <p className="text-sm text-muted-foreground">
                      Permanently remove {resource.name} and its deployment history.
                    </p>
                  </div>
                  {/* Gated like every sibling panel — it used to render for
                      Developers and then throw a raw permission error. */}
                  {canManage && (
                    <DeleteResourceButton
                      resourceId={resource.id}
                      name={resource.name}
                      kind={resource.kind}
                      ephemeral={"ephemeral" in resource ? Boolean(resource.ephemeral) : false}
                    />
                  )}
                </div>
              </CardContent>
            </Card>
          </div>
        </TabsContent>
      </Tabs>
    </div>
  );
}
