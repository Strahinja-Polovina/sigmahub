"use client";

import * as React from "react";
import {
  CircleCheck,
  CircleAlert,
  FileCode2,
  Boxes,
  Wand2,
  Copy,
  Check,
} from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";
import {
  BUILD_COMPOSE,
  BUILD_DOCKERFILE,
  BUILD_METHOD_HINTS,
  BUILD_METHOD_LABELS,
  BUILD_NIXPACKS,
  starterDockerfile,
  subdirError,
  type BuildDecision,
  type BuildMethod,
  type DetectedRepo,
} from "@/lib/wizard/build";
import {
  ROLLOUT_RECREATE,
  ignoredHostPorts,
  recreateSummary,
} from "@/lib/deploy-spec";

const METHOD_ICONS: Record<BuildMethod, React.ElementType> = {
  [BUILD_DOCKERFILE]: FileCode2,
  [BUILD_COMPOSE]: Boxes,
  [BUILD_NIXPACKS]: Wand2,
};

/**
 * The detected build, presented as a decision (SIGMA-209).
 *
 * Three things this screen has to do that the old one did not: show the
 * evidence behind the verdict, let the user override it, and — when detection
 * found nothing — offer a way forward instead of "not deployable, go away".
 */
export function BuildStep({
  detected,
  decision,
  method,
  onMethodChange,
  dockerfile,
  onDockerfileChange,
  contextSubdir,
  onContextSubdirChange,
}: {
  detected: DetectedRepo | null;
  decision: BuildDecision;
  method: BuildMethod | null;
  onMethodChange: (m: BuildMethod) => void;
  dockerfile: string;
  onDockerfileChange: (v: string) => void;
  contextSubdir: string;
  onContextSubdirChange: (v: string) => void;
}) {
  const [copied, setCopied] = React.useState(false);
  const services = detected?.services ?? [];
  const overridden = method !== null && method !== decision.method;
  const subdirProblem = subdirError(contextSubdir);

  const starter = React.useMemo(
    () => starterDockerfile(detected?.language, detected?.ports?.[0] ?? 3000),
    [detected?.language, detected?.ports]
  );

  return (
    <div className="flex flex-col gap-4">
      {/* The verdict and its evidence. */}
      <div
        className={cn(
          "flex items-start gap-3 rounded-lg border p-3",
          decision.method
            ? "border-emerald-500/30 bg-emerald-500/5"
            : "border-amber-500/40 bg-amber-500/5"
        )}
      >
        {decision.method ? (
          <CircleCheck className="mt-0.5 size-4 shrink-0 text-emerald-600" />
        ) : (
          <CircleAlert className="mt-0.5 size-4 shrink-0 text-amber-600" />
        )}
        <div className="min-w-0">
          <p className="text-sm font-medium text-foreground">{decision.headline}</p>
          <p className="font-mono text-xs text-muted-foreground">{decision.evidence}</p>
          <p className="mt-1 text-xs text-muted-foreground">{decision.detail}</p>
        </div>
      </div>

      {/* Switching is always available. Detection is a best-effort read of
          someone else's repository; being confidently wrong about it must not
          be terminal. */}
      <div className="flex flex-col gap-1.5">
        <Label className="text-xs text-muted-foreground">Build method</Label>
        <div className="grid gap-1.5 sm:grid-cols-3">
          {decision.alternatives.map((m) => {
            const Icon = METHOD_ICONS[m];
            const selected = method === m;
            return (
              <button
                key={m}
                type="button"
                onClick={() => onMethodChange(m)}
                aria-pressed={selected}
                className={cn(
                  "flex flex-col gap-1 rounded-lg border px-3 py-2.5 text-left transition-colors",
                  selected
                    ? "border-primary bg-primary/5 ring-1 ring-primary/20"
                    : "border-border bg-card hover:bg-muted/50"
                )}
              >
                <span className="flex items-center gap-2">
                  <Icon className="size-3.5 shrink-0 text-muted-foreground" />
                  <span className="text-sm font-medium text-foreground">
                    {BUILD_METHOD_LABELS[m]}
                  </span>
                  {m === decision.method && (
                    <Badge variant="outline" className="ml-auto text-[10px]">
                      detected
                    </Badge>
                  )}
                </span>
                <span className="text-xs leading-snug text-muted-foreground">
                  {BUILD_METHOD_HINTS[m]}
                </span>
              </button>
            );
          })}
        </div>
        {overridden && (
          <p className="text-xs text-amber-600">
            You&apos;ve overridden what was detected. The deploy will fail if the
            repository doesn&apos;t match — check the paths below.
          </p>
        )}
      </div>

      {/* Dockerfile path + build context. Both have always travelled to the
          agent's build op; the single-container path just never set either, so
          a repo whose Dockerfile is not at its root could not be deployed. */}
      {method === BUILD_DOCKERFILE && (
        <div className="grid gap-3 sm:grid-cols-2">
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="wizard-dockerfile" className="text-xs text-muted-foreground">
              Dockerfile
            </Label>
            <Input
              id="wizard-dockerfile"
              value={dockerfile}
              onChange={(e) => onDockerfileChange(e.target.value)}
              placeholder="Dockerfile"
              className="font-mono"
              spellCheck={false}
            />
            <p className="text-xs text-muted-foreground">Relative to the build context.</p>
          </div>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="wizard-context" className="text-xs text-muted-foreground">
              Build context
            </Label>
            <Input
              id="wizard-context"
              value={contextSubdir}
              onChange={(e) => onContextSubdirChange(e.target.value)}
              placeholder="repository root"
              className="font-mono"
              spellCheck={false}
              aria-invalid={subdirProblem ? true : undefined}
            />
            <p className={cn("text-xs", subdirProblem ? "text-destructive" : "text-muted-foreground")}>
              {subdirProblem ?? "A subdirectory, for a monorepo. Empty means the repository root."}
            </p>
          </div>
        </div>
      )}

      {method === BUILD_NIXPACKS && (
        <div className="flex flex-col gap-1.5">
          <Label htmlFor="wizard-nix-context" className="text-xs text-muted-foreground">
            Build context
          </Label>
          <Input
            id="wizard-nix-context"
            value={contextSubdir}
            onChange={(e) => onContextSubdirChange(e.target.value)}
            placeholder="repository root"
            className="font-mono"
            spellCheck={false}
            aria-invalid={subdirProblem ? true : undefined}
          />
          <p className={cn("text-xs", subdirProblem ? "text-destructive" : "text-muted-foreground")}>
            {subdirProblem ??
              "Nixpacks reads the project manifest in this directory to derive the toolchain and start command."}
          </p>
        </div>
      )}

      {/* The Compose graph. Showing it is the point: the operator has to see
          that every service was understood, and which of them cannot swap
          without going down. */}
      {method === BUILD_COMPOSE && services.length > 0 && (
        <div className="flex flex-col gap-2 rounded-lg border border-border bg-card p-3">
          <p className="text-xs text-muted-foreground">
            {services.length} {services.length === 1 ? "service" : "services"} will be
            deployed, each as its own container. After it&apos;s created you can move
            each one onto its own server.
          </p>
          <ul className="flex flex-col gap-1.5">
            {services.map((svc) => (
              <li key={svc.name} className="flex flex-wrap items-center gap-1.5">
                <span className="font-mono text-sm text-foreground">{svc.name}</span>
                <Badge variant="outline" className="font-mono text-[10px]">
                  {svc.build ? `build ${svc.build}` : svc.image}
                </Badge>
                {(svc.ports?.length ?? 0) > 0 && (
                  <span className="text-xs text-muted-foreground tabular-nums">
                    :{svc.ports!.join(", :")}
                  </span>
                )}
                {svc.rollout === ROLLOUT_RECREATE && (
                  <Badge variant="outline" className="border-amber-500/40 text-[10px] text-amber-600">
                    recreate
                  </Badge>
                )}
              </li>
            ))}
          </ul>
          {recreateSummary(services).length > 0 && (
            <div className="flex items-start gap-2 rounded-md border border-amber-500/30 bg-amber-500/5 p-2.5">
              <CircleAlert className="mt-0.5 size-3.5 shrink-0 text-amber-600" />
              <div className="min-w-0 text-xs text-muted-foreground">
                <p className="font-medium text-foreground">
                  Not every service deploys with zero downtime.
                </p>
                <ul className="mt-1 flex flex-col gap-0.5">
                  {recreateSummary(services).map((svc) => (
                    <li key={svc.name}>
                      <span className="font-mono text-foreground">{svc.name}</span> is
                      stopped before its replacement starts, because {svc.reason}.
                    </li>
                  ))}
                </ul>
              </div>
            </div>
          )}
          {ignoredHostPorts(services).length > 0 && (
            <div className="flex items-start gap-2 rounded-md border border-border bg-muted/40 p-2.5">
              <CircleAlert className="mt-0.5 size-3.5 shrink-0 text-muted-foreground" />
              <div className="min-w-0 text-xs text-muted-foreground">
                <p className="font-medium text-foreground">Host port bindings are not used.</p>
                <ul className="mt-1 flex flex-col gap-0.5">
                  {ignoredHostPorts(services).map((svc) => (
                    <li key={svc.name}>
                      <span className="font-mono text-foreground">{svc.name}</span> asks for
                      host port{svc.ports.length > 1 ? "s" : ""} {svc.ports.join(", ")}.
                      Traefik routes to the container instead, so nothing binds the host —
                      attach a domain to reach it from outside.
                    </li>
                  ))}
                </ul>
              </div>
            </div>
          )}
        </div>
      )}

      {method === BUILD_COMPOSE && services.length === 0 && (
        <p className="rounded-lg border border-dashed border-border p-3 text-xs text-muted-foreground">
          No compose file was detected in this repository. Point the build context at
          the directory that holds one, or pick a different build method.
        </p>
      )}

      {/* The dead end, made survivable. */}
      {!decision.method && (
        <div className="flex flex-col gap-2 rounded-lg border border-border bg-muted/40 p-3">
          <div className="flex items-center justify-between gap-2">
            <p className="text-sm font-medium text-foreground">
              Starter Dockerfile
              {detected?.languageLabel ? ` for ${detected.languageLabel}` : ""}
            </p>
            <Button
              variant="outline"
              size="sm"
              onClick={() => {
                void navigator.clipboard?.writeText(starter);
                setCopied(true);
                window.setTimeout(() => setCopied(false), 2000);
              }}
            >
              {copied ? <Check className="size-3.5" /> : <Copy className="size-3.5" />}
              {copied ? "Copied" : "Copy"}
            </Button>
          </div>
          <pre className="max-h-48 overflow-auto rounded-md border border-border bg-background p-2.5 font-mono text-[11px] leading-relaxed text-foreground">
            {starter}
          </pre>
          <p className="text-xs text-muted-foreground">
            Commit this at the repository root, then read the repository again. Or
            switch to auto-build above and let nixpacks work it out.
          </p>
        </div>
      )}
    </div>
  );
}
