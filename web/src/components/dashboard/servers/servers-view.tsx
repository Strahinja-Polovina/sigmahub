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
import { agentCheckIn } from "@/server/actions/servers";
import { ConnectServerDialog } from "./connect-server-dialog";
import { SERVER_TYPE_LABELS, SERVER_TYPE_ORDER } from "./server-meta";

type Filter = "all" | ServerType;

function TypeBadge({ type }: { type: ServerType }) {
  return (
    <Badge variant="outline" className="font-normal">
      {SERVER_TYPE_LABELS[type]}
    </Badge>
  );
}

export function CheckInButton({ serverId }: { serverId: string }) {
  const [pending, startTransition] = React.useTransition();
  return (
    <Button
      variant="outline"
      size="sm"
      disabled={pending}
      onClick={() =>
        startTransition(async () => {
          try {
            await agentCheckIn({ serverId });
            toast.success("Agent checked in", {
              description: "The server is now running and billable.",
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
      Simulate check-in
    </Button>
  );
}

function ServerRow({ server }: { server: ServerWithCount }) {
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
          {server.status === "provisioning" && <CheckInButton serverId={server.id} />}
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
}: {
  orgId: string;
  orgName: string;
  orgSlug: string;
  servers: ServerWithCount[];
}) {
  const [filter, setFilter] = React.useState<Filter>("all");

  const counts = React.useMemo(() => {
    const c: Record<Filter, number> = {
      all: servers.length,
      general: 0,
      database: 0,
      storage: 0,
      gpu: 0,
    };
    for (const sv of servers) c[sv.type as ServerType] += 1;
    return c;
  }, [servers]);

  const filtered =
    filter === "all" ? servers : servers.filter((sv) => sv.type === filter);
  const connected = servers.filter((sv) => sv.status !== "provisioning").length;

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
        <ConnectServerDialog orgId={orgId} orgSlug={orgSlug} />
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
              {SERVER_TYPE_ORDER.map((t) => (
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
                    <TableHead>IP</TableHead>
                    <TableHead>Resources</TableHead>
                    <TableHead className="pr-4">Access</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {filtered.map((sv) => (
                    <ServerRow key={sv.id} server={sv} />
                  ))}
                </TableBody>
              </Table>
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  );
}
