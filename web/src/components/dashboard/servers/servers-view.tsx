"use client";

import * as React from "react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { Server as ServerIcon, ShieldCheck, Boxes, Clock, Loader2, Radio } from "lucide-react";
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
import {
  demoTeardownPhase,
  forceReason,
  isDecommissioning,
  msUntilNextTeardownStep,
} from "@/lib/decommission";
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

/** Demo-only: the graceful teardown actually happening.
 *
 *  Pressing Disconnect used to leave the row in `decommissioning` forever,
 *  because the thing that ends it is an agent ack and demo mode has no agent.
 *  The only way out was a simulate button the user had to notice — a demo of a
 *  transient state that never transitioned.
 *
 *  So the default outcome runs on a clock, and the clock walks the agent's real
 *  uninstall sequence step by step (see demoTeardownPhase). This component is
 *  what asks for the render that shows each step, and it is what writes the ack
 *  at the end: nothing runs between requests, so if the page is not watching,
 *  nothing finishes it. The other endings stay as buttons next to it — a
 *  teardown that fails, or an agent that never answers, are not things a timer
 *  can produce, and they are the two that lead to the force path.
 *
 *  It acks ONLY a teardown it watched start. Writing the ack claims the agent
 *  reported in, and a component that writes it on arrival is asserting
 *  something it never observed: the demo clock is seven to ten seconds of
 *  absolute wall time, so any row already past it at mount — the seeded
 *  fixture, a tab opened a minute late, a row the "…never answers" button has
 *  just backdated — is a teardown nobody saw finish. Acking those deleted a
 *  server the visitor had not touched, on first paint, dropped the fleet by one
 *  and popped a green toast for it; on the timeout button it deleted the very
 *  server whose force
 *  path that button exists to demonstrate. Those rows render as what they
 *  honestly are — in flight, unconfirmed — with both simulate buttons beside
 *  them and the dialog's force path a click away. */
function TeardownProgress({
  serverId,
  status,
  startedAt,
  purgeVolumes,
}: {
  serverId: string;
  status: string;
  startedAt: Date | string | null;
  purgeVolumes: boolean;
}) {
  const router = useRouter();
  // The clock is the source of truth and the state is only a reason to look at
  // it again, so the phase is DERIVED on render rather than stored: a copy in
  // state would need an effect to keep it correct, and the two would disagree
  // for one frame every time a step landed.
  const [, retick] = React.useState(0);
  const phase = demoTeardownPhase({ startedAt, purgeVolumes });
  // Whether this teardown was still running the first time this component laid
  // eyes on it — the one thing that entitles it to write the ack later.
  //
  // Keyed on the timestamp, because a decommissionStartedAt that CHANGES is a
  // different request: "…never answers" rewrites the row to eleven minutes ago,
  // and a component holding its earlier answer would read the backdated row as
  // finished and ack it. Answered during render rather than in an effect, so no
  // frame ever exists in which an already-finished teardown looks watched.
  const key = startedAt instanceof Date ? startedAt.getTime() : (startedAt ?? "unset");
  const [watched, setWatched] = React.useState(() => ({ key, fromStart: !phase.done }));
  if (watched.key !== key) setWatched({ key, fromStart: !phase.done });
  const fromStart = watched.key === key ? watched.fromStart : !phase.done;
  // And past the control plane's window, the graceful path has had its chance:
  // whatever the step clock says, what is true about that row is that nothing
  // answered. Force disconnect and the cleanup script are the next honest move,
  // not a tombstone written on the agent's behalf.
  const timedOut = forceReason({ status, decommissioningSince: startedAt }) !== null;
  const watching = fromStart && !timedOut;
  const ackedRef = React.useRef(false);

  React.useEffect(() => {
    if (!watching) return;
    if (phase.done) {
      // Once, even if this effect re-runs: the ack DELETES the server row, and
      // a second call would fail with "Server not found" against a row the
      // first one legitimately removed.
      if (ackedRef.current) return;
      ackedRef.current = true;
      void simulateDecommission({ serverId, event: "ack" })
        .then(() => {
          toast.success("Decommissioned", {
            description: "The agent removed everything it installed and then itself.",
          });
          router.refresh();
        })
        .catch(() => {
          // Another tab, or the "…with errors" button, got there first. The row
          // is gone either way, which is the outcome this was driving toward.
        });
      return;
    }
    const wait = msUntilNextTeardownStep({ startedAt, purgeVolumes });
    if (wait === null) return;
    const timer = setTimeout(() => retick((n) => n + 1), wait + 100);
    return () => clearTimeout(timer);
  }, [serverId, startedAt, purgeVolumes, watching, phase.done, phase.step, router]);

  // No spinner: nothing is turning. The request is out and the record is
  // waiting, which is the true state of every teardown this page arrived after.
  if (!watching) {
    return (
      <span className="inline-flex items-center gap-1.5 text-xs text-muted-foreground">
        <Clock className="size-3.5" />
        <span>The agent has not confirmed the teardown</span>
      </span>
    );
  }

  return (
    <span className="inline-flex items-center gap-1.5 text-xs text-muted-foreground">
      <Loader2 className="size-3.5 animate-spin" />
      <span>
        {phase.label} ({Math.min(phase.step + 1, phase.total)}/{phase.total})
      </span>
    </span>
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
              {/* The default ending, happening — for a teardown this page
                  watched begin, which is the one the visitor just asked for. It
                  finishes by itself; the buttons beside it are the two that a
                  clock cannot produce, and they are also the way out of a
                  teardown whose seconds were already spent when the page
                  arrived. */}
              <TeardownProgress
                serverId={server.id}
                status={server.status}
                startedAt={server.decommissionStartedAt}
                purgeVolumes={server.decommissionPurgeVolumes}
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

      {/* Both modes. It was gated on cpMode, so with no control plane the
          product's whole cluster story — build one from your own servers, then
          deploy to it like a server — was invisible to exactly the audience
          demo mode exists for (SIGMA-215). */}
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
        simulated={!cpMode}
      />
    </div>
  );
}
