"use client";

import * as React from "react";
import Link from "next/link";
import { Server as ServerIcon, ShieldCheck, Boxes } from "lucide-react";

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
import { StatusBadge } from "@/components/dashboard/status-indicator";
import { useActiveOrg } from "@/components/dashboard/org-context";
import { getServers } from "@/lib/mock";
import type { Server, ServerType } from "@/lib/mock";
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

function ServerRow({ server }: { server: Server }) {
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
        <TypeBadge type={server.type} />
      </TableCell>
      <TableCell className="text-muted-foreground">
        <span className="text-foreground">{server.provider}</span>
        <span className="text-muted-foreground/60"> · {server.region}</span>
      </TableCell>
      <TableCell>
        <StatusBadge status={server.status} />
      </TableCell>
      <TableCell className="font-mono text-xs text-muted-foreground tabular-nums">
        {server.agentVersion}
      </TableCell>
      <TableCell className="font-mono text-xs text-muted-foreground tabular-nums">
        {server.ip}
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

export function ServersView() {
  const { orgId, org } = useActiveOrg();
  const [filter, setFilter] = React.useState<Filter>("all");

  const servers = React.useMemo(() => getServers(orgId), [orgId]);

  const counts = React.useMemo(() => {
    const c: Record<Filter, number> = {
      all: servers.length,
      general: 0,
      database: 0,
      storage: 0,
      gpu: 0,
    };
    for (const s of servers) c[s.type] += 1;
    return c;
  }, [servers]);

  const filtered = React.useMemo(
    () => (filter === "all" ? servers : servers.filter((s) => s.type === filter)),
    [servers, filter]
  );

  const connected = React.useMemo(
    () => servers.filter((s) => s.status !== "provisioning").length,
    [servers]
  );

  return (
    <div className="flex flex-col gap-6 p-4 md:p-6">
      <div className="flex flex-col justify-between gap-3 sm:flex-row sm:items-start">
        <div className="flex flex-col gap-1">
          <h1 className="text-xl font-semibold tracking-tight text-foreground">
            Servers
          </h1>
          <p className="text-sm text-muted-foreground">
            {connected} connected across {org.name} · your infrastructure,
            single billing meter.
          </p>
        </div>
        <ConnectServerDialog />
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
          <Tabs
            value={filter}
            onValueChange={(v) => setFilter(v as Filter)}
            className="w-fit"
          >
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
                No servers of this type
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
                  {filtered.map((s) => (
                    <ServerRow key={s.id} server={s} />
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
