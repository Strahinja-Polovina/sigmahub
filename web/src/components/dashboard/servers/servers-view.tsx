"use client";

import * as React from "react";
import Link from "next/link";
import { Server as ServerIcon, ShieldCheck, Boxes, Loader2, Radio } from "lucide-react";
import { toast } from "sonner";

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
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { StatusBadge } from "@/components/dashboard/status-indicator";
import type { ServerType, Status } from "@/lib/mock";
import type { ServerWithCount } from "@/server/queries";
import {
  agentCheckIn,
  simulateDecommission,
  type DemoHostShape,
} from "@/server/actions/servers";
import { isDecommissioning } from "@/lib/decommission";
import {
  ClustersPanel,
  type ClusterEnvironment,
} from "@/components/dashboard/clusters/clusters-panel";
import type { CpCluster } from "@/server/cp";
import { ConnectServerDialog } from "./connect-server-dialog";
import {
  SERVER_TYPE_LABELS,
  SERVER_TYPES,
} from "@/lib/server-catalog.generated";

type Filter = "all" | ServerType;

function TypeBadge({ type }: { type: ServerType }) {
  return (
    <Badge variant="outline" className="font-normal">
      {SERVER_TYPE_LABELS[type]}
    </Badge>
  );
}

/** Demo-only agent check-in.
 *
 *  `shape` is what the pretend machine turns out to be. "generic" is an
 *  ordinary box — no accelerator, a small disk — which is what someone actually
 *  plugs in when they picked GPU or Storage by mistake, and the only way to
 *  reach (and then recover from) the incompatible state without owning the
 *  wrong hardware (SIGMA-203/215). */
export function CheckInButton({
  serverId,
  shape = "matching",
  label = "Simulate check-in",
}: {
  serverId: string;
  shape?: DemoHostShape;
  label?: string;
}) {
  const [pending, startTransition] = React.useTransition();
  return (
    <Button
      variant="outline"
      size="sm"
      disabled={pending}
      onClick={() =>
        startTransition(async () => {
          try {
            await agentCheckIn({ serverId, shape });
            toast.success("Agent checked in", {
              description:
                shape === "generic"
                  ? "The host reported an ordinary box — see how it lands against the type you picked."
                  : "The server is now running and billable.",
            });
          } catch (err) {
            toast.error("Check-in failed", {
              description: err instanceof Error ? err.message : "Please try again.",
            });
          }
        })
      }
    >
      {pending ? <Loader2 className="size-3.5 animate-spin" /> : <Radio className="size-3.5" />}
      {label}
    </Button>
  );
}

/** Demo-only: drive an in-flight decommission to one of its real endings.
 *
 *  Demo mode is where someone learns what these states mean, so it has to walk
 *  the whole flow — including the endings where the agent does NOT come back,
 *  which are the ones that produce the force path and the manual cleanup script
 *  (SIGMA-215). Absent in CP mode: there a real sigmad answers, or does not. */
function DecommissionSimButton({
  serverId,
  event,
  label,
  description,
}: {
  serverId: string;
  event: "ack" | "failed" | "timeout" | "silence";
  label: string;
  description: string;
}) {
  const [pending, startTransition] = React.useTransition();
  return (
    <Button
      variant="outline"
      size="sm"
      disabled={pending}
      onClick={() =>
        startTransition(async () => {
          try {
            await simulateDecommission({ serverId, event });
            toast.success(label, { description });
          } catch (err) {
            toast.error("Simulation failed", {
              description: err instanceof Error ? err.message : "Please try again.",
            });
          }
        })
      }
    >
      {pending ? <Loader2 className="size-3.5 animate-spin" /> : <Radio className="size-3.5" />}
      {label}
    </Button>
  );
}

function ServerRow({ server, cpMode }: { server: ServerWithCount; cpMode?: boolean }) {
  return (
    <TableRow>
      <TableCell className="pl-4">
        <Link
          href={`/dashboard/servers/${server.id}`}
          className="font-medium text-foreground hover:underline"
        >
          {server.name}
        </Link>
      </TableCell>
      <TableCell>
        <TypeBadge type={server.type as ServerType} />
      </TableCell>
      <TableCell className="text-muted-foreground">
        <span className="text-foreground">{server.provider}</span>
        <span className="text-muted-foreground/60"> · {server.region}</span>
      </TableCell>
      <TableCell>
        <div className="flex items-center gap-2">
          <StatusBadge status={server.status as Status} />
          {/* CP mode: the real sigmad checks in — nothing to simulate. */}
          {!cpMode && server.status === "provisioning" && (
            <>
              <CheckInButton serverId={server.id} />
              <CheckInButton serverId={server.id} shape="generic" label="…as an ordinary box" />
            </>
          )}
          {/* A refused host recovers the moment the machine satisfies the
              requirement — a driver installed, a disk grown. In demo mode the
              equivalent is checking in as the machine the type expects. */}
          {!cpMode && server.status === "incompatible" && (
            <CheckInButton serverId={server.id} label="…as the right machine" />
          )}
          {/* A teardown in flight, and the three ways it ends: the agent
              confirms, the agent confirms a failure it could not fix, or the
              agent never answers and the control plane's timeout takes over —
              which is the state the force path and the cleanup script exist
              for (SIGMA-204/205). */}
          {!cpMode && isDecommissioning(server.status) && (
            <>
              <DecommissionSimButton
                serverId={server.id}
                event="ack"
                label="Agent confirms"
                description="The host is clean and the server has been removed."
              />
              <DecommissionSimButton
                serverId={server.id}
                event="failed"
                label="…with errors"
                description="The server is removed and the audit log says what did not tear down."
              />
              <DecommissionSimButton
                serverId={server.id}
                event="timeout"
                label="…never answers"
                description="Open Disconnect again — it now offers Force disconnect and the cleanup script."
              />
            </>
          )}
          {/* The other route to the force path: a machine that stopped
              answering cannot be asked to uninstall anything. */}
          {!cpMode && server.status === "running" && (
            <DecommissionSimButton
              serverId={server.id}
              event="silence"
              label="Simulate silence"
              description="The server is now unreachable — Disconnect offers the force path."
            />
          )}
        </div>
      </TableCell>
      <TableCell className="font-mono text-xs text-muted-foreground tabular-nums">
        {server.agentVersion || "—"}
      </TableCell>
      <TableCell className="font-mono text-xs text-muted-foreground tabular-nums">
        {server.ip || "—"}
      </TableCell>
      <TableCell className="text-muted-foreground tabular-nums">
        {server.resourceCount}
      </TableCell>
      <TableCell className="pr-4">
        {server.byoVpn ? (
          <Badge variant="outline" className="gap-1 font-normal">
            <ShieldCheck className="size-3 text-muted-foreground" />
            VPN
          </Badge>
        ) : (
          <span className="text-muted-foreground/40">—</span>
        )}
      </TableCell>
    </TableRow>
  );
}

export function ServersView({
  orgId,
  orgName,
  orgSlug,
  servers,
  cpMode,
  clusters = [],
  clusterExcludedKinds = [],
  clusterEnvironments = [],
}: {
  orgId: string;
  orgName: string;
  orgSlug: string;
  servers: ServerWithCount[];
  cpMode?: boolean;
  /** Kubernetes clusters built from these servers (CP mode). */
  clusters?: CpCluster[];
  clusterExcludedKinds?: string[];
  clusterEnvironments?: ClusterEnvironment[];
}) {
  const [filter, setFilter] = React.useState<Filter>("all");

  const counts = React.useMemo(() => {
    // Seeded from the control plane's own type list so adding a server type
    // can't leave a counter undefined (and crash the filter row) the way a
    // hand-written literal would. An unknown type still counts toward "all".
    const c = { all: servers.length } as Record<Filter, number>;
    for (const t of SERVER_TYPES) c[t] = 0;
    for (const sv of servers) {
      if (sv.type in c) c[sv.type as Filter] += 1;
    }
    return c;
  }, [servers]);

  const filtered =
    filter === "all" ? servers : servers.filter((sv) => sv.type === filter);
  // "Connected" means actually reporting — matching the CP's ConnectedServerCount
  // and the billing read model. Counting everything that isn't `provisioning`
  // also counted servers the CP had already flipped to unreachable (SIGMA-184).
  const connected = servers.filter((sv) => sv.status === "running").length;

  return (
    <div className="flex flex-col gap-6 p-4 md:p-6">
      <div className="flex flex-col justify-between gap-3 sm:flex-row sm:items-start">
        <div className="flex flex-col gap-1">
          <h1 className="text-xl font-semibold tracking-tight text-foreground">Servers</h1>
          <p className="text-sm text-muted-foreground">
            {connected} connected across {orgName} · your infrastructure, single
            billing meter.
          </p>
        </div>
        <ConnectServerDialog orgId={orgId} orgSlug={orgSlug} cpMode={cpMode} />
      </div>

      <Card>
        <CardHeader className="gap-3 border-b sm:flex-row sm:items-center sm:justify-between">
          <div className="flex flex-col gap-1">
            <CardTitle className="flex items-center gap-2">
              <ServerIcon className="size-4 text-muted-foreground" />
              Connected servers
            </CardTitle>
            <CardDescription>
              Hosts running the SigmaHub agent over WireGuard.
            </CardDescription>
          </div>
          <Tabs value={filter} onValueChange={(v) => setFilter(v as Filter)} className="w-fit">
            <TabsList>
              <TabsTrigger value="all">All ({counts.all})</TabsTrigger>
              {SERVER_TYPES.map((t) => (
                <TabsTrigger key={t} value={t} disabled={counts[t] === 0}>
                  {SERVER_TYPE_LABELS[t]} ({counts[t]})
                </TabsTrigger>
              ))}
            </TabsList>
          </Tabs>
        </CardHeader>
        <CardContent className="px-0">
          {filtered.length === 0 ? (
            <div className="flex flex-col items-center gap-2 px-4 py-14 text-center">
              <span className="grid size-10 place-items-center rounded-lg bg-muted text-muted-foreground">
                <Boxes className="size-5" />
              </span>
              <p className="text-sm font-medium text-foreground">
                {servers.length === 0 ? "No servers connected yet" : "No servers of this type"}
              </p>
              <p className="max-w-sm text-xs text-muted-foreground">
                Connect a host and pick a type when the agent checks in.
              </p>
            </div>
          ) : (
            <div className="overflow-x-auto">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead className="pl-4">Name</TableHead>
                    <TableHead>Type</TableHead>
                    <TableHead>Provider · Region</TableHead>
                    <TableHead>Status</TableHead>
                    <TableHead>Agent</TableHead>
                    <TableHead>Public IP</TableHead>
                    <TableHead>Resources</TableHead>
                    <TableHead className="pr-4">Access</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {filtered.map((sv) => (
                    <ServerRow key={sv.id} server={sv} cpMode={cpMode} />
                  ))}
                </TableBody>
              </Table>
            </div>
          )}
        </CardContent>
      </Card>

      {cpMode && (
        <ClustersPanel
          orgId={orgId}
          clusters={clusters}
          excludedKinds={clusterExcludedKinds}
          servers={servers.map((sv) => ({
            id: sv.id,
            name: sv.name,
            type: sv.type,
            status: sv.status,
          }))}
          environments={clusterEnvironments}
          canManage
        />
      )}
    </div>
  );
}
