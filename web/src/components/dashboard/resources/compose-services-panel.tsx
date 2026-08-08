"use client";

import * as React from "react";
import { useRouter } from "next/navigation";
import { toast } from "sonner";
import { Boxes, Loader2, Server as ServerIcon, ArrowRight, Save, Info } from "lucide-react";

import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { setComposePlacements } from "@/server/actions/compose";
import { ROLLOUT_RECREATE, recreateReason } from "@/lib/deploy-spec";
import type { CpComposeService } from "@/server/cp";

const HOME = "__home__";

export type PlacementServer = { id: string; name: string; type: string };

/**
 * Per-service placement for a Compose app.
 *
 * A Compose app is a graph, and there is no reason every service has to share
 * one host: the database wants a database server, the web tier wants the proxy
 * edge. Services that depend on a service placed elsewhere are held back by the
 * control plane until that dependency is up, so an app never starts against a
 * database that isn't running.
 */
export function ComposeServicesPanel({
  orgId,
  resourceId,
  services,
  homeServerId,
  servers,
  canManage,
}: {
  orgId: string;
  resourceId: string;
  services: CpComposeService[];
  homeServerId: string;
  servers: PlacementServer[];
  canManage: boolean;
}) {
  const router = useRouter();
  const [pending, startTransition] = React.useTransition();
  const [draft, setDraft] = React.useState<Record<string, { serverId: string; env: string }>>(
    () =>
      Object.fromEntries(
        services.map((s) => [
          s.name,
          {
            serverId: s.serverId || HOME,
            env: envToText(s.env),
          },
        ])
      )
  );
  const [envError, setEnvError] = React.useState<Record<string, string>>({});

  const serverName = React.useCallback(
    (id: string) => servers.find((s) => s.id === id)?.name ?? id,
    [servers]
  );

  const dirty = services.some((s) => {
    const d = draft[s.name];
    if (!d) return false;
    return d.serverId !== (s.serverId || HOME) || d.env !== envToText(s.env);
  });

  function save() {
    const placements: { service: string; serverId: string; env?: Record<string, string> }[] = [];
    const errors: Record<string, string> = {};

    for (const svc of services) {
      const d = draft[svc.name];
      if (!d) continue;
      const parsed = parseEnv(d.env);
      if ("error" in parsed) {
        errors[svc.name] = parsed.error;
        continue;
      }
      placements.push({
        service: svc.name,
        serverId: d.serverId === HOME ? "" : d.serverId,
        env: Object.keys(parsed.env).length > 0 ? parsed.env : undefined,
      });
    }
    setEnvError(errors);
    if (Object.keys(errors).length > 0) {
      toast.error("Fix the environment lines first");
      return;
    }

    startTransition(async () => {
      try {
        const { servers: affected } = await setComposePlacements({
          orgId,
          resourceId,
          placements,
        });
        toast.success("Placement saved", {
          description:
            affected.length > 1
              ? `Redeploying across ${affected.length} servers.`
              : "Redeploying with the new placement.",
        });
        router.refresh();
      } catch (err) {
        toast.error("Couldn’t save placement", {
          description: err instanceof Error ? err.message : "Please try again.",
        });
      }
    });
  }

  return (
    <Card>
      <CardHeader className="border-b">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="flex flex-col gap-1.5">
            <CardTitle className="inline-flex items-center gap-2">
              <Boxes className="size-4" />
              Services
            </CardTitle>
            <CardDescription>
              {services.length} {services.length === 1 ? "service" : "services"} from this
              repository&apos;s compose file. Each can run on its own server with its own
              environment.
            </CardDescription>
          </div>
          {canManage && dirty && (
            <Button size="sm" onClick={save} disabled={pending} className="shrink-0">
              {pending ? <Loader2 className="size-4 animate-spin" /> : <Save className="size-4" />}
              Save placement
            </Button>
          )}
        </div>
      </CardHeader>

      <CardContent className="flex flex-col gap-4 pt-4">
        {services.map((svc) => {
          const d = draft[svc.name] ?? { serverId: HOME, env: "" };
          const remoteDeps = (svc.dependsOn ?? []).filter((dep) => {
            const depServer = draft[dep]?.serverId ?? HOME;
            return depServer !== d.serverId;
          });
          return (
            <div
              key={svc.name}
              className="flex flex-col gap-3 rounded-lg border border-border p-3"
            >
              <div className="flex flex-wrap items-center gap-2">
                <span className="font-mono text-sm font-medium text-foreground">{svc.name}</span>
                {svc.build ? (
                  <Badge variant="outline" className="text-[10px]">
                    built from {svc.build === "." ? "repo root" : svc.build}
                  </Badge>
                ) : (
                  <Badge variant="outline" className="font-mono text-[10px]">
                    {svc.image}
                  </Badge>
                )}
                {svc.rollout === ROLLOUT_RECREATE && (
                  <Badge variant="outline" className="border-amber-500/40 text-[10px] text-amber-600">
                    recreate
                  </Badge>
                )}
                {(svc.ports?.length ?? 0) > 0 && (
                  <span className="text-xs text-muted-foreground tabular-nums">
                    :{svc.ports?.join(", :")}
                  </span>
                )}
              </div>

              {/* The badge alone reads as an arbitrary product decision. This
                  service is the exception to the zero-downtime promise the rest
                  of the UI makes, so it has to say which exclusive resource
                  makes it one. */}
              {recreateReason(svc) && (
                <p className="text-xs text-amber-600">
                  Deploys with a brief outage: stopped before its replacement starts, because{" "}
                  {recreateReason(svc)}.
                </p>
              )}

              <div className="grid gap-3 sm:grid-cols-2">
                <div className="flex flex-col gap-1.5">
                  <Label className="text-xs text-muted-foreground">Runs on</Label>
                  <Select
                    value={d.serverId}
                    onValueChange={(v) =>
                      setDraft((prev) => ({
                        ...prev,
                        [svc.name]: { ...d, serverId: v ?? HOME },
                      }))
                    }
                    disabled={!canManage}
                  >
                    <SelectTrigger>
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value={HOME}>
                        Default — {homeServerId ? serverName(homeServerId) : "the app’s server"}
                      </SelectItem>
                      {servers.map((s) => (
                        <SelectItem key={s.id} value={s.id}>
                          {s.name} · {s.type}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                </div>

                <div className="flex flex-col gap-1.5">
                  <Label
                    htmlFor={`env-${svc.name}`}
                    className="text-xs text-muted-foreground"
                  >
                    Environment <span className="opacity-70">(KEY=value per line)</span>
                  </Label>
                  <Textarea
                    id={`env-${svc.name}`}
                    value={d.env}
                    onChange={(e) =>
                      setDraft((prev) => ({ ...prev, [svc.name]: { ...d, env: e.target.value } }))
                    }
                    placeholder={"DB_HOST=10.8.0.4\nLOG_LEVEL=debug"}
                    className="min-h-16 font-mono text-xs"
                    spellCheck={false}
                    disabled={!canManage}
                  />
                  {envError[svc.name] && (
                    <p className="text-xs text-destructive">{envError[svc.name]}</p>
                  )}
                </div>
              </div>

              {(svc.dependsOn?.length ?? 0) > 0 && (
                <p className="flex flex-wrap items-center gap-1.5 text-xs text-muted-foreground">
                  <ArrowRight className="size-3.5 shrink-0" />
                  Starts after{" "}
                  <span className="font-mono text-foreground">{svc.dependsOn?.join(", ")}</span>
                  {remoteDeps.length > 0 && (
                    <>
                      {" "}
                      — {remoteDeps.length === 1 ? "that service runs" : "those services run"} on
                      another server, so this one waits until{" "}
                      {remoteDeps.length === 1 ? "it reports" : "they report"} healthy.
                    </>
                  )}
                </p>
              )}
            </div>
          );
        })}

        <p className="flex items-start gap-2 text-xs text-muted-foreground">
          <Info className="mt-0.5 size-3.5 shrink-0" />
          Services reach each other by name over the app&apos;s private network when they share
          a server. Across servers, use the target server&apos;s mesh address — visible on the{" "}
          <ServerIcon className="inline size-3" /> server page.
        </p>
      </CardContent>
    </Card>
  );
}

function envToText(env: Record<string, string> | undefined): string {
  if (!env) return "";
  return Object.entries(env)
    .map(([k, v]) => `${k}=${v}`)
    .join("\n");
}

/** Parse KEY=value lines. Blank lines and # comments are ignored. */
function parseEnv(text: string): { env: Record<string, string> } | { error: string } {
  const env: Record<string, string> = {};
  const lines = text.split("\n");
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i].trim();
    if (!line || line.startsWith("#")) continue;
    const eq = line.indexOf("=");
    if (eq < 1) {
      return { error: `Line ${i + 1}: expected KEY=value.` };
    }
    const key = line.slice(0, eq).trim();
    if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(key)) {
      return { error: `Line ${i + 1}: “${key}” is not a valid variable name.` };
    }
    env[key] = line.slice(eq + 1).trim();
  }
  return { env };
}
