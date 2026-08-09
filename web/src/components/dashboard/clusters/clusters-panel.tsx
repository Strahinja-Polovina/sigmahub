"use client";

import * as React from "react";
import { useRouter } from "next/navigation";
import { toast } from "sonner";
import {
  Boxes,
  Crown,
  Loader2,
  Plus,
  ServerIcon,
  ShieldAlert,
  Trash2,
  X,
} from "lucide-react";

import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { StatusDot } from "@/components/dashboard/status-indicator";
import type { Status } from "@/lib/mock";
import { resourceKindLabel } from "@/lib/server-catalog.generated";
import {
  createCluster,
  addClusterNode,
  removeClusterNode,
  deleteCluster,
} from "@/server/actions/clusters";
import type { CpCluster } from "@/server/cp";

export type ClusterServer = { id: string; name: string; type: string; status: string };

export type ClusterEnvironment = { id: string; name: string; projectName: string };

/** What a node last reported about Kubernetes on it. Kept separate from the
 *  server's own status: the agent checking in says nothing about whether k3s
 *  installed, joined, or is serving. */
const nodeTone: Record<string, Status> = {
  ready: "running",
  pending: "provisioning",
  error: "degraded",
};
const nodeLabel: Record<string, string> = {
  ready: "Kubernetes ready",
  pending: "joining",
  error: "not joined",
};


/**
 * Kubernetes clusters built from the org's own servers.
 *
 * One server becomes the control plane (it runs the API server); the rest join
 * as workers. Databases deliberately cannot run inside a cluster — a stateful
 * engine rescheduled onto a node without its data is data loss, so managed
 * databases stay on their own server and the cluster reaches them over the mesh.
 */
export function ClustersPanel({
  orgId,
  clusters,
  excludedKinds,
  servers,
  environments,
  canManage,
}: {
  orgId: string;
  clusters: CpCluster[];
  excludedKinds: string[];
  servers: ClusterServer[];
  environments: ClusterEnvironment[];
  canManage: boolean;
}) {
  const [createOpen, setCreateOpen] = React.useState(false);

  // A server already in a cluster can't join another one.
  const claimed = new Set(clusters.flatMap((c) => c.nodes.map((n) => n.serverId)));
  const available = servers.filter((s) => !claimed.has(s.id));

  // The excluded kinds are excluded for two unrelated reasons, and the panel
  // gives each its own sentence rather than one that is only true of the
  // stateful ones.
  const statefulExcluded = excludedKinds
    .filter((k) => k !== "llm")
    .map((k) => resourceKindLabel(k))
    .sort();

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex flex-col gap-1">
          <h2 className="text-base font-semibold text-foreground">Kubernetes clusters</h2>
          <p className="text-sm text-muted-foreground">
            Promote your own servers into a cluster: one control plane, any number of
            workers. Deploy to it the same way you deploy to a server.
          </p>
        </div>
        {canManage && environments.length > 0 && (
          <Button size="sm" onClick={() => setCreateOpen(true)} className="shrink-0">
            <Plus className="size-4" />
            New cluster
          </Button>
        )}
      </div>

      {clusters.length === 0 ? (
        <Card>
          <CardContent className="flex flex-col items-center gap-2 py-10 text-center">
            <Boxes className="size-6 text-muted-foreground" />
            <p className="text-sm text-muted-foreground">
              No clusters yet. A cluster needs at least one connected server to act as its
              control plane.
            </p>
          </CardContent>
        </Card>
      ) : (
        clusters.map((cluster) => (
          <ClusterCard
            key={cluster.id}
            orgId={orgId}
            cluster={cluster}
            available={available}
            canManage={canManage}
          />
        ))
      )}

      {excludedKinds.length > 0 && (
        <Alert>
          <ShieldAlert className="size-4" />
          <AlertTitle>Some resources run outside the cluster, on purpose</AlertTitle>
          <AlertDescription>
            {statefulExcluded.length > 0 && (
              <>
                {statefulExcluded.join(", ")} stay on their own server. A stateful engine
                rescheduled onto a node without its data is data loss, not a slow deploy —
                so your app runs in the cluster and reaches its database over the mesh,
                exactly as it would from any other server.
              </>
            )}
            {/* A model endpoint is not a database and loses nothing it cannot
                re-download. Folding it into the sentence above told operators
                their inference server was at risk of data loss and left the
                actual reason — no cluster render path — unsaid. */}
            {excludedKinds.includes("llm") && (
              <>
                {statefulExcluded.length > 0 && " "}
                {resourceKindLabel("llm")} runs on a GPU server of its own for a different
                reason: the scheduler has no path for a model endpoint yet.
              </>
            )}
          </AlertDescription>
        </Alert>
      )}

      {canManage && (
        <CreateClusterDialog
          open={createOpen}
          onOpenChange={setCreateOpen}
          orgId={orgId}
          servers={available}
          environments={environments}
        />
      )}
    </div>
  );
}

function ClusterCard({
  orgId,
  cluster,
  available,
  canManage,
}: {
  orgId: string;
  cluster: CpCluster;
  available: ClusterServer[];
  canManage: boolean;
}) {
  const router = useRouter();
  const [pending, startTransition] = React.useTransition();
  const [addId, setAddId] = React.useState("");

  function run(fn: () => Promise<void>, ok: string) {
    startTransition(async () => {
      try {
        await fn();
        toast.success(ok);
        router.refresh();
      } catch (err) {
        toast.error("Couldn’t update the cluster", {
          description: err instanceof Error ? err.message : "Please try again.",
        });
      }
    });
  }

  const statusTone: Record<string, Status> = {
    ready: "running",
    provisioning: "provisioning",
    degraded: "degraded",
  };

  return (
    <Card>
      <CardHeader className="border-b">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="flex flex-col gap-1.5">
            <CardTitle className="inline-flex items-center gap-2">
              <Boxes className="size-4" />
              {cluster.name}
              <Badge variant="outline" className="gap-1.5 font-normal">
                <StatusDot status={statusTone[cluster.status] ?? "unknown"} />
                {cluster.status}
              </Badge>
            </CardTitle>
            <CardDescription>
              {cluster.nodes.length} {cluster.nodes.length === 1 ? "node" : "nodes"}
              {cluster.apiEndpoint && (
                <>
                  {" · API "}
                  <span className="font-mono text-xs">{cluster.apiEndpoint}</span>
                </>
              )}
              {cluster.kubernetesVersion && ` · ${cluster.kubernetesVersion}`}
            </CardDescription>
          </div>
          {canManage && (
            <DeleteClusterButton
              pending={pending}
              onConfirm={() =>
                run(() => deleteCluster({ orgId, clusterId: cluster.id }), "Cluster deleted")
              }
              name={cluster.name}
            />
          )}
        </div>
      </CardHeader>

      <CardContent className="flex flex-col gap-3 pt-4">
        <ul className="flex flex-col divide-y divide-border">
          {cluster.nodes.map((node) => {
            const isControlPlane = node.role === "control-plane";
            return (
              <li
                key={node.serverId}
                className="flex flex-wrap items-center justify-between gap-3 py-2.5 first:pt-0 last:pb-0"
              >
                <div className="flex min-w-0 flex-col gap-1">
                  <div className="flex min-w-0 flex-wrap items-center gap-2">
                    {/* The node's own report about Kubernetes, not the agent's
                        heartbeat: an agent can be checking in perfectly on a
                        host where k3s never installed, and showing one as the
                        other is how a cluster looks fine while nothing can be
                        scheduled on it. */}
                    <StatusDot status={nodeTone[node.nodeStatus] ?? "unknown"} />
                    <span className="truncate text-sm font-medium text-foreground">
                      {node.serverName}
                    </span>
                    {isControlPlane ? (
                      <Badge variant="outline" className="gap-1 text-[10px]">
                        <Crown className="size-3" />
                        control plane
                      </Badge>
                    ) : (
                      <Badge variant="outline" className="text-[10px]">
                        worker
                      </Badge>
                    )}
                    <Badge variant="outline" className="text-[10px]">
                      {nodeLabel[node.nodeStatus] ?? node.nodeStatus}
                    </Badge>
                    {node.meshIp && (
                      <span className="font-mono text-xs text-muted-foreground">{node.meshIp}</span>
                    )}
                  </div>
                  {node.nodeMessage && (
                    <p className="text-xs text-amber-700 dark:text-amber-500">
                      {node.nodeMessage}
                    </p>
                  )}
                </div>
                {canManage && !isControlPlane && (
                  <Button
                    variant="ghost"
                    size="sm"
                    disabled={pending}
                    onClick={() =>
                      run(
                        () =>
                          removeClusterNode({
                            orgId,
                            clusterId: cluster.id,
                            serverId: node.serverId,
                          }),
                        "Node removed"
                      )
                    }
                  >
                    <X className="size-4" />
                    Remove
                  </Button>
                )}
              </li>
            );
          })}
        </ul>

        {canManage && available.length > 0 && (
          <div className="flex flex-wrap items-end gap-2">
            <div className="flex min-w-48 flex-1 flex-col gap-1.5">
              <Label className="text-xs text-muted-foreground">Add a worker node</Label>
              <Select value={addId} onValueChange={(v) => setAddId(v ?? "")}>
                <SelectTrigger>
                  <SelectValue placeholder="Choose a server" />
                </SelectTrigger>
                <SelectContent>
                  {available.map((s) => (
                    <SelectItem key={s.id} value={s.id}>
                      {s.name} · {s.type}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <Button
              size="sm"
              disabled={!addId || pending}
              onClick={() =>
                run(async () => {
                  await addClusterNode({ orgId, clusterId: cluster.id, serverId: addId });
                  setAddId("");
                }, "Node joining the cluster")
              }
            >
              {pending ? <Loader2 className="size-4 animate-spin" /> : <Plus className="size-4" />}
              Join
            </Button>
          </div>
        )}
      </CardContent>
    </Card>
  );
}

function DeleteClusterButton({
  name,
  pending,
  onConfirm,
}: {
  name: string;
  pending: boolean;
  onConfirm: () => void;
}) {
  const [open, setOpen] = React.useState(false);
  return (
    <>
      <Button variant="ghost" size="sm" className="shrink-0" onClick={() => setOpen(true)}>
        <Trash2 className="size-4" />
        Delete
      </Button>
      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete {name}?</DialogTitle>
            <DialogDescription>
              Kubernetes is torn down on every node. Apps deployed into this cluster are
              not deleted — they lose their target and stop running until you point them
              at a server or another cluster.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <DialogClose render={<Button variant="outline" />}>Cancel</DialogClose>
            <Button
              variant="destructive"
              disabled={pending}
              onClick={() => {
                onConfirm();
                setOpen(false);
              }}
            >
              <Trash2 className="size-4" />
              Delete cluster
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  );
}

function CreateClusterDialog({
  open,
  onOpenChange,
  orgId,
  servers,
  environments,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
  orgId: string;
  servers: ClusterServer[];
  environments: ClusterEnvironment[];
}) {
  const router = useRouter();
  const [name, setName] = React.useState("");
  const [environmentId, setEnvironmentId] = React.useState("");
  const [controlPlaneId, setControlPlaneId] = React.useState("");
  const [pending, startTransition] = React.useTransition();

  function submit() {
    startTransition(async () => {
      try {
        await createCluster({ orgId, environmentId, name, controlPlaneId });
        toast.success("Cluster created", {
          description: "The control-plane node is installing Kubernetes now.",
        });
        setName("");
        setControlPlaneId("");
        onOpenChange(false);
        router.refresh();
      } catch (err) {
        toast.error("Couldn’t create the cluster", {
          description: err instanceof Error ? err.message : "Please try again.",
        });
      }
    });
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>New cluster</DialogTitle>
          <DialogDescription>
            One of your servers becomes the control plane and runs the Kubernetes API on
            its mesh address — never on a public interface.
          </DialogDescription>
        </DialogHeader>

        <div className="flex flex-col gap-4 py-2">
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="cluster-name">Name</Label>
            <Input
              id="cluster-name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="production"
              className="font-mono"
            />
          </div>

          <div className="flex flex-col gap-1.5">
            <Label htmlFor="cluster-env">Environment</Label>
            <Select value={environmentId} onValueChange={(v) => setEnvironmentId(v ?? "")}>
              <SelectTrigger id="cluster-env">
                <SelectValue placeholder="Choose an environment" />
              </SelectTrigger>
              <SelectContent>
                {environments.map((e) => (
                  <SelectItem key={e.id} value={e.id}>
                    {e.projectName} · {e.name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <p className="text-xs text-muted-foreground">
              One cluster per environment, so “deploy to the cluster” is unambiguous.
            </p>
          </div>

          <div className="flex flex-col gap-1.5">
            <Label htmlFor="cluster-cp">Control-plane server</Label>
            <Select value={controlPlaneId} onValueChange={(v) => setControlPlaneId(v ?? "")}>
              <SelectTrigger id="cluster-cp">
                <SelectValue placeholder="Choose a server" />
              </SelectTrigger>
              <SelectContent>
                {servers.map((s) => (
                  <SelectItem key={s.id} value={s.id}>
                    {s.name} · {s.type}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            {servers.length === 0 && (
              <p className="flex items-center gap-1.5 text-xs text-muted-foreground">
                <ServerIcon className="size-3.5" />
                Every connected server already belongs to a cluster.
              </p>
            )}
          </div>
        </div>

        <DialogFooter>
          <DialogClose render={<Button variant="outline" />}>Cancel</DialogClose>
          <Button
            disabled={pending || !name.trim() || !environmentId || !controlPlaneId}
            onClick={submit}
          >
            {pending ? <Loader2 className="size-4 animate-spin" /> : <Boxes className="size-4" />}
            Create cluster
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
