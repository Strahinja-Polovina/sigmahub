"use client";

import * as React from "react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { toast } from "sonner";
import {
  ArrowLeft,
  Cpu,
  MemoryStick,
  Network,
  ShieldCheck,
  CalendarDays,
  Boxes,
  MoreHorizontal,
  RotateCw,
  MinusCircle,
  Unplug,
  Activity,
  Loader2,
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
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Separator } from "@/components/ui/separator";
import { StatusBadge, StatusDot } from "@/components/dashboard/status-indicator";
import type { ResourceKind, ServerType, Status } from "@/lib/mock";
import { disconnectServer } from "@/server/actions/servers";
import { ServerMetrics } from "./server-metrics";
import { CheckInButton } from "./servers-view";
import {
  SERVER_TYPE_LABELS,
  RESOURCE_KIND_LABELS,
  formatDate,
} from "./server-meta";

type ServerRowT = {
  id: string;
  name: string;
  type: string;
  provider: string;
  region: string;
  status: string;
  agentVersion: string;
  ip: string;
  cpu: number;
  memGb: number;
  byoVpn: boolean;
  connectedAt: Date;
  orgId: string;
};

type HostedRow = {
  id: string;
  name: string;
  kind: string;
  status: string;
  projectName: string;
  envName: string;
};

function SpecItem({
  icon: Icon,
  label,
  value,
}: {
  icon: React.ElementType;
  label: string;
  value: React.ReactNode;
}) {
  return (
    <div className="flex items-center gap-3">
      <span className="grid size-9 shrink-0 place-items-center rounded-lg bg-muted text-muted-foreground">
        <Icon className="size-4" />
      </span>
      <div className="flex flex-col">
        <span className="text-xs text-muted-foreground">{label}</span>
        <span className="text-sm font-medium text-foreground">{value}</span>
      </div>
    </div>
  );
}

function ServerActions({ serverId, serverName }: { serverId: string; serverName: string }) {
  const router = useRouter();
  const [pending, startTransition] = React.useTransition();

  function disconnect() {
    startTransition(async () => {
      try {
        await disconnectServer({ serverId });
        toast.success(`Disconnected ${serverName}`, {
          description: "The agent tears down its WireGuard tunnel.",
        });
        router.push("/dashboard/servers");
      } catch (err) {
        toast.error("Couldn’t disconnect", {
          description: err instanceof Error ? err.message : "Please try again.",
        });
      }
    });
  }

  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        render={
          <Button variant="outline" size="sm" aria-label="Server actions" disabled={pending}>
            {pending ? <Loader2 className="animate-spin" /> : <MoreHorizontal />}
            Actions
          </Button>
        }
      />
      <DropdownMenuContent align="end" className="w-48">
        <DropdownMenuItem
          className="gap-2"
          onClick={() => toast.success(`Restarting agent on ${serverName}…`)}
        >
          <RotateCw className="size-4 text-muted-foreground" />
          Restart agent
        </DropdownMenuItem>
        <DropdownMenuItem
          className="gap-2"
          onClick={() =>
            toast(`Cordoned ${serverName}`, {
              description: "No new resources will be scheduled here.",
            })
          }
        >
          <MinusCircle className="size-4 text-muted-foreground" />
          Cordon
        </DropdownMenuItem>
        <DropdownMenuSeparator />
        <DropdownMenuItem variant="destructive" className="gap-2" onClick={disconnect}>
          <Unplug className="size-4" />
          Disconnect
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

function HostedResourceRow({ resource }: { resource: HostedRow }) {
  return (
    <TableRow>
      <TableCell className="pl-4 font-medium text-foreground">
        <span className="inline-flex items-center gap-2">
          <StatusDot status={resource.status as Status} />
          {resource.name}
        </span>
      </TableCell>
      <TableCell>
        <Badge variant="outline" className="font-mono font-normal">
          {RESOURCE_KIND_LABELS[resource.kind as ResourceKind]}
        </Badge>
      </TableCell>
      <TableCell className="text-muted-foreground">
        {resource.projectName}
        <span className="text-muted-foreground/60"> / {resource.envName}</span>
      </TableCell>
      <TableCell className="pr-4">
        <StatusBadge status={resource.status as Status} />
      </TableCell>
    </TableRow>
  );
}

export function ServerDetailView({
  server,
  hosted,
}: {
  server: ServerRowT;
  hosted: HostedRow[];
}) {
  const provisioning = server.status === "provisioning";

  return (
    <div className="flex flex-col gap-6 p-4 md:p-6">
      <div className="flex flex-col gap-4">
        <Link
          href="/dashboard/servers"
          className="inline-flex w-fit items-center gap-1.5 text-xs text-muted-foreground hover:text-foreground"
        >
          <ArrowLeft className="size-3.5" />
          Servers
        </Link>

        <div className="flex flex-col justify-between gap-4 sm:flex-row sm:items-start">
          <div className="flex flex-col gap-2">
            <div className="flex flex-wrap items-center gap-3">
              <h1 className="font-mono text-xl font-semibold tracking-tight text-foreground">
                {server.name}
              </h1>
              <StatusBadge status={server.status as Status} />
              {server.byoVpn && (
                <Badge variant="outline" className="gap-1 font-normal">
                  <ShieldCheck className="size-3 text-muted-foreground" />
                  VPN / jump host
                </Badge>
              )}
            </div>
            <p className="flex flex-wrap items-center gap-x-2 gap-y-1 text-sm text-muted-foreground">
              <Badge variant="outline" className="font-normal">
                {SERVER_TYPE_LABELS[server.type as ServerType]}
              </Badge>
              <span>·</span>
              <span className="text-foreground">{server.provider}</span>
              <span>·</span>
              <span>{server.region}</span>
              {server.ip && (
                <>
                  <span>·</span>
                  <span className="font-mono text-xs">{server.ip}</span>
                </>
              )}
            </p>
          </div>
          <div className="flex items-center gap-2">
            {provisioning && <CheckInButton serverId={server.id} />}
            <ServerActions serverId={server.id} serverName={server.name} />
          </div>
        </div>
      </div>

      <div className="grid grid-cols-1 gap-4 lg:grid-cols-3">
        <Card className="lg:col-span-2">
          <CardHeader className="border-b">
            <CardTitle className="flex items-center gap-2">
              <Activity className="size-4 text-muted-foreground" />
              Metrics
            </CardTitle>
            <CardDescription>Last 24 hours reported by the agent.</CardDescription>
          </CardHeader>
          <CardContent>
            {provisioning ? (
              <div className="flex flex-col items-center gap-2 py-12 text-center">
                <Loader2 className="size-5 animate-spin text-muted-foreground" />
                <p className="text-sm font-medium text-foreground">Waiting for the agent</p>
                <p className="max-w-sm text-xs text-muted-foreground">
                  Metrics stream in once the agent checks in.
                </p>
              </div>
            ) : (
              <ServerMetrics seedKey={server.id} />
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader className="border-b">
            <CardTitle>Specification</CardTitle>
            <CardDescription>Reported host capacity.</CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-4">
            <SpecItem icon={Cpu} label="vCPU" value={provisioning ? "—" : `${server.cpu} cores`} />
            <SpecItem icon={MemoryStick} label="Memory" value={provisioning ? "—" : `${server.memGb} GB`} />
            <Separator />
            <SpecItem
              icon={Network}
              label="Agent version"
              value={<span className="font-mono text-sm">{server.agentVersion || "—"}</span>}
            />
            <SpecItem icon={CalendarDays} label="Connected" value={formatDate(server.connectedAt)} />
            <SpecItem
              icon={ShieldCheck}
              label="Access"
              value={server.byoVpn ? "Via VPN / jump host" : "Direct outbound WireGuard"}
            />
          </CardContent>
        </Card>
      </div>

      <Card>
        <CardHeader className="border-b">
          <CardTitle className="flex items-center gap-2">
            <Boxes className="size-4 text-muted-foreground" />
            Hosted resources
          </CardTitle>
          <CardDescription>
            Resources scheduled onto this server across all environments.
          </CardDescription>
        </CardHeader>
        <CardContent className="px-0">
          {hosted.length === 0 ? (
            <div className="flex flex-col items-center gap-2 px-4 py-12 text-center">
              <span className="grid size-10 place-items-center rounded-lg bg-muted text-muted-foreground">
                <Boxes className="size-5" />
              </span>
              <p className="text-sm font-medium text-foreground">Nothing scheduled here yet</p>
              <p className="max-w-sm text-xs text-muted-foreground">
                {provisioning
                  ? "This server is still provisioning."
                  : "Deploy a resource to this server to see it here."}
              </p>
            </div>
          ) : (
            <div className="overflow-x-auto">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead className="pl-4">Name</TableHead>
                    <TableHead>Type</TableHead>
                    <TableHead>Project / Environment</TableHead>
                    <TableHead className="pr-4">Status</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {hosted.map((r) => (
                    <HostedResourceRow key={r.id} resource={r} />
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
