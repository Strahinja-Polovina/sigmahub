"use client";

import * as React from "react";
import { Boxes, Server as ServerIcon, Check, Network, CircleAlert } from "lucide-react";

import { Label } from "@/components/ui/label";
import { Badge } from "@/components/ui/badge";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { cn } from "@/lib/utils";
import { SERVER_TYPE_LABELS, type ResourceKind, type ServerType } from "@/lib/server-catalog.generated";
import {
  clusterOptions,
  serverOptions,
  type TargetInventory,
  type WizardCluster,
  type WizardProject,
} from "@/lib/wizard/availability";
import type { ModelCard } from "@/lib/wizard/llm";

/**
 * Project → Environment → Server OR Cluster (SIGMA-210).
 *
 * The cluster half is new to the UI and not to the product: clusterId has
 * threaded end to end since SIGMA-200 and there has simply been no control that
 * could set it, so every cluster an org built was unreachable from the deploy
 * flow. The picker is filtered to the chosen environment because a cluster
 * belongs to exactly one, and offering the others would offer a target the
 * control plane refuses.
 */
export function TargetStep({
  kind,
  projects,
  clusters,
  inventory,
  model,
  projectId,
  environmentId,
  serverId,
  clusterId,
  onProjectChange,
  onEnvironmentChange,
  onServerChange,
  onClusterChange,
}: {
  kind: ResourceKind;
  projects: WizardProject[];
  clusters: WizardCluster[];
  inventory: TargetInventory;
  /** The model an `llm` resource will serve (SIGMA-214). Every other kind
   *  passes nothing and is filtered exactly as before. */
  model?: ModelCard | null;
  projectId: string;
  environmentId: string;
  serverId: string;
  clusterId: string;
  onProjectChange: (id: string) => void;
  onEnvironmentChange: (id: string) => void;
  onServerChange: (id: string) => void;
  onClusterChange: (id: string) => void;
}) {
  const environments = React.useMemo(
    () => projects.find((p) => p.id === projectId)?.environments ?? [],
    [projects, projectId]
  );
  const env = environments.find((e) => e.id === environmentId);
  const servers = React.useMemo(() => serverOptions(env, kind, model), [env, kind, model]);
  const clusterChoices = React.useMemo(
    () => clusterOptions(clusters, environmentId, kind, inventory),
    [clusters, environmentId, kind, inventory]
  );

  const noEligibleServer =
    Boolean(environmentId) && servers.length > 0 && servers.every((s) => !s.eligible);

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-col gap-1.5">
        <Label htmlFor="wizard-project">
          <Boxes className="size-3.5 text-muted-foreground" />
          Project
        </Label>
        <Select value={projectId} onValueChange={(v) => onProjectChange((v as string) ?? "")}>
          <SelectTrigger id="wizard-project" className="w-full">
            <SelectValue placeholder="Select a project">
              {(v) => projects.find((p) => p.id === v)?.name ?? "Select a project"}
            </SelectValue>
          </SelectTrigger>
          <SelectContent>
            {projects.map((p) => (
              <SelectItem key={p.id} value={p.id}>
                {p.name}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>

      <div className="flex flex-col gap-1.5">
        <Label htmlFor="wizard-env">Environment</Label>
        <Select
          value={environmentId}
          onValueChange={(v) => onEnvironmentChange((v as string) ?? "")}
          disabled={!projectId}
        >
          <SelectTrigger id="wizard-env" className="w-full">
            <SelectValue placeholder="Select an environment">
              {(v) => environments.find((e) => e.id === v)?.name ?? "Select an environment"}
            </SelectValue>
          </SelectTrigger>
          <SelectContent>
            {environments.map((e) => (
              <SelectItem key={e.id} value={e.id}>
                {e.name}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>

      <div className="flex flex-col gap-1.5">
        <Label>
          <ServerIcon className="size-3.5 text-muted-foreground" />
          Runs on
        </Label>

        {!environmentId ? (
          <p className="rounded-lg border border-dashed border-border px-3 py-4 text-center text-xs text-muted-foreground">
            Choose an environment to see where this can run.
          </p>
        ) : servers.length === 0 && clusterChoices.length === 0 ? (
          <p className="rounded-lg border border-dashed border-border px-3 py-4 text-center text-xs text-muted-foreground">
            No servers or clusters are attached to this environment. Attach one
            from the environment&rsquo;s settings, or pick a different
            environment above.
          </p>
        ) : (
          <div className="flex flex-col gap-1.5">
            {servers.map(({ server, eligible, reason }) => {
              const selected = serverId === server.id;
              return (
                <button
                  key={server.id}
                  type="button"
                  disabled={!eligible}
                  aria-pressed={selected}
                  onClick={() => eligible && onServerChange(server.id)}
                  className={cn(
                    "flex items-start gap-3 rounded-lg border px-3 py-2.5 text-left transition-colors",
                    !eligible && "cursor-not-allowed border-border bg-muted/40",
                    eligible &&
                      (selected
                        ? "border-primary bg-primary/5 ring-1 ring-primary/20"
                        : "border-border bg-card hover:bg-muted/50")
                  )}
                >
                  <ServerIcon
                    className={cn(
                      "mt-0.5 size-4 shrink-0",
                      eligible ? "text-muted-foreground" : "text-muted-foreground/50"
                    )}
                  />
                  <span className="min-w-0 flex-1">
                    <span className="flex items-center gap-2">
                      <span
                        className={cn(
                          "truncate font-mono text-sm",
                          eligible ? "text-foreground" : "text-muted-foreground"
                        )}
                      >
                        {server.name}
                      </span>
                      <Badge variant="outline" className="shrink-0 text-[10px]">
                        {SERVER_TYPE_LABELS[server.type as ServerType] ?? server.type}
                      </Badge>
                    </span>
                    {/* Disabled-with-reason, everywhere. A greyed-out row whose
                        cause is a matrix the user has never seen is the pattern
                        this flow is replacing — and a reason clipped at one line
                        is the same thing with extra steps, so only the
                        provider/region caption truncates. */}
                    <span
                      className={cn(
                        "block text-xs text-muted-foreground",
                        !reason && "truncate"
                      )}
                    >
                      {reason ?? [server.provider, server.region].filter(Boolean).join(" · ")}
                    </span>
                  </span>
                  {selected && <Check className="mt-0.5 size-4 shrink-0 text-primary" />}
                </button>
              );
            })}

            {clusterChoices.map(({ cluster, eligible, reason }) => {
              const selected = clusterId === cluster.id;
              return (
                <button
                  key={cluster.id}
                  type="button"
                  disabled={!eligible}
                  aria-pressed={selected}
                  onClick={() => eligible && onClusterChange(cluster.id)}
                  className={cn(
                    "flex items-start gap-3 rounded-lg border px-3 py-2.5 text-left transition-colors",
                    !eligible && "cursor-not-allowed border-border bg-muted/40",
                    eligible &&
                      (selected
                        ? "border-primary bg-primary/5 ring-1 ring-primary/20"
                        : "border-border bg-card hover:bg-muted/50")
                  )}
                >
                  <Network
                    className={cn(
                      "mt-0.5 size-4 shrink-0",
                      eligible ? "text-muted-foreground" : "text-muted-foreground/50"
                    )}
                  />
                  <span className="min-w-0 flex-1">
                    <span className="flex items-center gap-2">
                      <span
                        className={cn(
                          "truncate font-mono text-sm",
                          eligible ? "text-foreground" : "text-muted-foreground"
                        )}
                      >
                        {cluster.name}
                      </span>
                      <Badge variant="outline" className="shrink-0 text-[10px]">
                        Cluster
                      </Badge>
                    </span>
                    <span className="block truncate text-xs text-muted-foreground">
                      {reason ?? "Kubernetes picks the node; the workload has no fixed host."}
                    </span>
                  </span>
                  {selected && <Check className="mt-0.5 size-4 shrink-0 text-primary" />}
                </button>
              );
            })}
          </div>
        )}

        {noEligibleServer && clusterChoices.every((c) => !c.eligible) && (
          <div className="flex items-start gap-2 rounded-lg border border-destructive/30 bg-destructive/5 p-3">
            <CircleAlert className="mt-0.5 size-4 shrink-0 text-destructive" />
            <p className="min-w-0 text-xs text-destructive/90">
              Nothing in this environment can host this resource. Attach a compatible
              server to it, or pick a different environment.
            </p>
          </div>
        )}
      </div>
    </div>
  );
}
