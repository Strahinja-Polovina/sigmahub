"use client";

import * as React from "react";
import { Globe, Plus, Trash2, CircleAlert, Lock } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { cn } from "@/lib/utils";
import {
  blankPortMapping,
  domainError,
  portMappingsError,
  reachability,
  type PortMapping,
} from "@/lib/wizard/networking";

/**
 * Ports, domain and health check (SIGMA-210).
 *
 * Every value here was detected and none was ever shown. The ports drive the
 * rollout's exposed ports AND the default health probe, so a wrong detection
 * the user could not correct was a first deploy that failed its gate with
 * nothing in the UI to explain why (SIGMA-160).
 */
export function NetworkStep({
  ports,
  onPortsChange,
  domain,
  onDomainChange,
  healthPath,
  onHealthPathChange,
  composeMode,
}: {
  ports: PortMapping[];
  onPortsChange: (ports: PortMapping[]) => void;
  domain: string;
  onDomainChange: (v: string) => void;
  healthPath: string;
  onHealthPathChange: (v: string) => void;
  /** A Compose app's ports belong to its services, not to one container. */
  composeMode: boolean;
}) {
  const portsProblem = portMappingsError(ports);
  const domainProblem = domainError(domain);
  const reach = reachability(ports, domain);

  function update(id: string, patch: Partial<PortMapping>) {
    onPortsChange(ports.map((p) => (p.id === id ? { ...p, ...patch } : p)));
  }

  return (
    <div className="flex flex-col gap-4">
      {composeMode ? (
        <p className="rounded-lg border border-border bg-muted/40 p-3 text-xs text-muted-foreground">
          Each Compose service exposes its own ports, so there is nothing to map here.
          Traefik reaches the web-facing service through the domain below.
        </p>
      ) : (
        <div className="flex flex-col gap-2">
          <Label className="text-xs text-muted-foreground">Ports</Label>
          <div className="flex flex-col gap-2">
            <div className="grid grid-cols-[1fr_1fr_auto] items-center gap-2 text-[11px] text-muted-foreground">
              <span>Container port</span>
              <span>Host port</span>
              <span className="sr-only">Remove</span>
            </div>
            {ports.map((p) => (
              <div key={p.id} className="grid grid-cols-[1fr_1fr_auto] items-center gap-2">
                <Input
                  value={p.container === 0 ? "" : String(p.container)}
                  onChange={(e) =>
                    update(p.id, { container: Number(e.target.value.replace(/\D/g, "")) || 0 })
                  }
                  inputMode="numeric"
                  placeholder="8080"
                  className="font-mono tabular-nums"
                  aria-label="Container port"
                />
                <Input
                  value={p.host === 0 ? "" : String(p.host)}
                  onChange={(e) =>
                    update(p.id, { host: Number(e.target.value.replace(/\D/g, "")) || 0 })
                  }
                  inputMode="numeric"
                  placeholder="internal only"
                  className="font-mono tabular-nums"
                  aria-label="Host port"
                />
                <Button
                  variant="ghost"
                  size="icon-sm"
                  aria-label={`Remove port ${p.container}`}
                  onClick={() => onPortsChange(ports.filter((x) => x.id !== p.id))}
                >
                  <Trash2 className="size-4 text-muted-foreground" />
                </Button>
              </div>
            ))}
            {ports.length === 0 && (
              <p className="rounded-lg border border-dashed border-border px-3 py-3 text-center text-xs text-muted-foreground">
                No ports detected. Add one if this app listens on a port.
              </p>
            )}
          </div>
          <Button
            variant="outline"
            size="sm"
            className="w-fit"
            onClick={() => onPortsChange([...ports, blankPortMapping()])}
          >
            <Plus className="size-3.5" />
            Add port
          </Button>
          {portsProblem && (
            <p className="flex items-center gap-1.5 text-xs text-destructive">
              <CircleAlert className="size-3.5 shrink-0" />
              {portsProblem}
            </p>
          )}
          <p className="text-xs text-muted-foreground">
            Leave the host port empty to keep a port internal — that is the safe
            default. Publishing one binds it on the machine, which collides the
            moment a second app wants it; a domain is the usual way in.
          </p>
        </div>
      )}

      <div className="flex flex-col gap-1.5">
        <Label htmlFor="wizard-domain">
          <Globe className="size-3.5 text-muted-foreground" />
          Domain <span className="text-xs font-normal text-muted-foreground">(optional)</span>
        </Label>
        <Input
          id="wizard-domain"
          value={domain}
          onChange={(e) => onDomainChange(e.target.value)}
          placeholder="app.example.com"
          className="font-mono"
          spellCheck={false}
          aria-invalid={domainProblem ? true : undefined}
        />
        <p className={cn("text-xs", domainProblem ? "text-destructive" : "text-muted-foreground")}>
          {domainProblem ??
            "Traefik terminates TLS and routes to the container. You can attach one later from the resource page, with DNS instructions."}
        </p>
      </div>

      {!composeMode && (
        <div className="flex flex-col gap-1.5">
          <Label htmlFor="wizard-health">Health check path</Label>
          <Input
            id="wizard-health"
            value={healthPath}
            onChange={(e) => onHealthPathChange(e.target.value)}
            placeholder="/healthz"
            className="font-mono"
            spellCheck={false}
          />
          <p className="text-xs text-muted-foreground">
            A new container has to answer here before traffic moves to it — that gate is
            what makes a deploy zero-downtime. Clear the field for a plain TCP check.
          </p>
        </div>
      )}

      <div
        className={cn(
          "flex items-start gap-2 rounded-lg border p-3 text-xs",
          reach.reachable
            ? "border-border bg-muted/40 text-muted-foreground"
            : "border-amber-500/30 bg-amber-500/5 text-muted-foreground"
        )}
      >
        {reach.reachable ? (
          <Globe className="mt-0.5 size-3.5 shrink-0" />
        ) : (
          <Lock className="mt-0.5 size-3.5 shrink-0 text-amber-600" />
        )}
        <p className="min-w-0">{reach.summary}</p>
      </div>
    </div>
  );
}
