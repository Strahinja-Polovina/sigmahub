"use client";

import * as React from "react";
import { useRouter } from "next/navigation";
import { toast } from "sonner";
import {
  GitBranch,
  FileCode2,
  Boxes,
  Server as ServerIcon,
  Check,
  ChevronRight,
  ChevronLeft,
  Plus,
  Trash2,
  Loader2,
  CircleCheck,
  CircleAlert,
  ArrowRight,
  Search,
} from "lucide-react";

import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogDescription,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Badge } from "@/components/ui/badge";
import { Progress } from "@/components/ui/progress";
import { cn } from "@/lib/utils";
import { canHost } from "@/lib/mock";
import type { ResourceKind, ServerType } from "@/lib/mock";
import { createResource } from "@/server/actions/resources";
import {
  KIND_LABELS,
  SERVER_TYPE_LABELS,
  type DeployTarget,
} from "./resource-meta";

// ---- Mock Git surface -------------------------------------------------------

type Repo = {
  fullName: string;
  description: string;
  private: boolean;
  defaultBranch: string;
  // The kind the detected build produces — drives the availability matrix.
  detectedKind: ResourceKind;
  build: "Dockerfile" | "docker-compose.yml";
  buildDetail: string;
  port: number;
};

const MOCK_REPOS: Repo[] = [
  {
    fullName: "acme/storefront",
    description: "Next.js customer-facing storefront",
    private: true,
    defaultBranch: "main",
    detectedKind: "app",
    build: "Dockerfile",
    buildDetail: "node:20-alpine · 3 stages",
    port: 3000,
  },
  {
    fullName: "acme/api",
    description: "Core REST + gRPC services",
    private: true,
    defaultBranch: "main",
    detectedKind: "app",
    build: "docker-compose.yml",
    buildDetail: "api + worker services",
    port: 8080,
  },
  {
    fullName: "acme/ml-inference",
    description: "vLLM inference server",
    private: true,
    defaultBranch: "main",
    detectedKind: "llm",
    build: "Dockerfile",
    buildDetail: "nvidia/cuda:12.4 base",
    port: 8000,
  },
  {
    fullName: "acme/edge-cache",
    description: "Redis-backed edge cache",
    private: false,
    defaultBranch: "main",
    detectedKind: "redis",
    build: "docker-compose.yml",
    buildDetail: "redis:7.4",
    port: 6379,
  },
];

type EnvVar = { id: string; key: string; value: string };

const STEPS = [
  { id: 1, label: "Repository" },
  { id: 2, label: "Build" },
  { id: 3, label: "Target" },
  { id: 4, label: "Variables" },
  { id: 5, label: "Deploy" },
] as const;

const DEPLOY_PHASES = [
  "Cloning repository",
  "Building image",
  "Pushing to registry",
  "Provisioning on server",
  "Running health checks",
] as const;

let seq = 0;
function newId() {
  seq += 1;
  return `envvar_${seq}`;
}

export function DeployWizard({
  open,
  onOpenChange,
  targets,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  targets: DeployTarget[];
}) {
  const router = useRouter();
  const [step, setStep] = React.useState(1);
  const [repoQuery, setRepoQuery] = React.useState("");
  const [repo, setRepo] = React.useState<Repo | null>(null);
  const [projectId, setProjectId] = React.useState<string>("");
  const [environmentId, setEnvironmentId] = React.useState<string>("");
  const [serverId, setServerId] = React.useState<string>("");
  const [envVars, setEnvVars] = React.useState<EnvVar[]>([]);
  const [deployPhase, setDeployPhase] = React.useState(0);
  const [deploying, setDeploying] = React.useState(false);

  const projects = targets;
  const kind = repo?.detectedKind;

  const environments = React.useMemo(
    () => projects.find((p) => p.id === projectId)?.environments ?? [],
    [projects, projectId]
  );

  // Candidate servers for the chosen environment, annotated with matrix compatibility.
  const candidateServers = React.useMemo(() => {
    const env = environments.find((e) => e.id === environmentId);
    if (!env || !kind) return [];
    return env.servers.map((server) => ({
      server,
      compatible: canHost(server.type as ServerType, kind),
    }));
  }, [environments, environmentId, kind]);

  const selectedServer = React.useMemo(
    () => environments.flatMap((e) => e.servers).find((s) => s.id === serverId),
    [environments, serverId]
  );

  // A typed "422-style" incompatibility error, shown inline in step 3.
  // Two cases: an explicitly-picked server that can't host the kind, or an
  // environment whose servers are all incompatible with the detected kind.
  const incompatibility = React.useMemo(() => {
    if (!kind) return null;
    if (selectedServer && !canHost(selectedServer.type as ServerType, kind)) {
      return {
        code: 422,
        type: "resource_kind_unsupported",
        message: `A ${SERVER_TYPE_LABELS[selectedServer.type as ServerType]} server cannot host a ${KIND_LABELS[kind]} resource.`,
      };
    }
    if (
      environmentId &&
      candidateServers.length > 0 &&
      candidateServers.every((c) => !c.compatible)
    ) {
      return {
        code: 422,
        type: "no_eligible_server",
        message: `No server in this environment can host a ${KIND_LABELS[kind]} resource. Attach a compatible server first.`,
      };
    }
    return null;
  }, [selectedServer, kind, environmentId, candidateServers]);

  const filteredRepos = React.useMemo(() => {
    const q = repoQuery.trim().toLowerCase();
    if (!q) return MOCK_REPOS;
    return MOCK_REPOS.filter(
      (r) =>
        r.fullName.toLowerCase().includes(q) ||
        r.description.toLowerCase().includes(q)
    );
  }, [repoQuery]);

  // Reset all state whenever the dialog is (re)opened.
  React.useEffect(() => {
    if (open) {
      setStep(1);
      setRepoQuery("");
      setRepo(null);
      setProjectId("");
      setEnvironmentId("");
      setServerId("");
      setEnvVars([
        { id: newId(), key: "NODE_ENV", value: "production" },
        { id: newId(), key: "", value: "" },
      ]);
      setDeployPhase(0);
      setDeploying(false);
    }
  }, [open]);

  // Drive the mock deploy animation once step 5 is reached.
  React.useEffect(() => {
    if (step !== 5) return;
    setDeploying(true);
    setDeployPhase(0);
    let phase = 0;
    const timer = setInterval(() => {
      phase += 1;
      if (phase >= DEPLOY_PHASES.length) {
        clearInterval(timer);
        setDeployPhase(DEPLOY_PHASES.length);
        // The animation is the simulated pipeline; persist the result now.
        createResource({
          projectId,
          environmentId,
          serverId,
          name: repo?.fullName.split("/").pop() ?? "resource",
          kind: repo?.detectedKind ?? "app",
          repo: repo?.fullName,
        })
          .then(() => {
            setDeploying(false);
            router.refresh();
            toast.success(`${repo?.fullName ?? "Resource"} deployed`, {
              description: "Health checks passed · now serving traffic.",
            });
          })
          .catch((err) => {
            setDeploying(false);
            toast.error("Deploy failed", {
              description: err instanceof Error ? err.message : "Please try again.",
            });
          });
      } else {
        setDeployPhase(phase);
      }
    }, 850);
    return () => clearInterval(timer);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [step]);

  const canNext = React.useMemo(() => {
    switch (step) {
      case 1:
        return Boolean(repo);
      case 2:
        return Boolean(repo);
      case 3:
        return (
          Boolean(projectId) &&
          Boolean(environmentId) &&
          Boolean(serverId) &&
          !incompatibility
        );
      case 4:
        return true;
      default:
        return false;
    }
  }, [step, repo, projectId, environmentId, serverId, incompatibility]);

  function next() {
    if (step < 5 && canNext) setStep((s) => s + 1);
  }
  function back() {
    if (step > 1 && step < 5) setStep((s) => s - 1);
  }

  function addEnvVar() {
    setEnvVars((v) => [...v, { id: newId(), key: "", value: "" }]);
  }
  function removeEnvVar(id: string) {
    setEnvVars((v) => v.filter((e) => e.id !== id));
  }
  function updateEnvVar(id: string, patch: Partial<EnvVar>) {
    setEnvVars((v) => v.map((e) => (e.id === id ? { ...e, ...patch } : e)));
  }

  const deployDone = step === 5 && !deploying && deployPhase >= DEPLOY_PHASES.length;

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        className="flex max-h-[85vh] flex-col gap-0 overflow-hidden p-0 sm:max-w-lg"
        showCloseButton={step !== 5}
      >
        <DialogHeader className="gap-1 border-b p-4">
          <DialogTitle className="flex items-center gap-2">
            <GitBranch className="size-4" />
            Deploy from Git
          </DialogTitle>
          <DialogDescription>
            Connect a repository and ship it to one of your environments.
          </DialogDescription>
        </DialogHeader>

        {/* Stepper */}
        <div className="flex items-center gap-1 border-b bg-muted/40 px-4 py-2.5">
          {STEPS.map((s, i) => {
            const active = s.id === step;
            const done = s.id < step;
            return (
              <React.Fragment key={s.id}>
                <div className="flex items-center gap-1.5">
                  <span
                    className={cn(
                      "grid size-5 shrink-0 place-items-center rounded-full border text-[10px] font-semibold tabular-nums",
                      active && "border-primary bg-primary text-primary-foreground",
                      done && "border-primary/30 bg-primary/10 text-primary",
                      !active && !done && "border-border bg-background text-muted-foreground"
                    )}
                  >
                    {done ? <Check className="size-3" /> : s.id}
                  </span>
                  <span
                    className={cn(
                      "hidden text-xs font-medium sm:inline",
                      active ? "text-foreground" : "text-muted-foreground"
                    )}
                  >
                    {s.label}
                  </span>
                </div>
                {i < STEPS.length - 1 && (
                  <span className="mx-0.5 h-px flex-1 bg-border" aria-hidden />
                )}
              </React.Fragment>
            );
          })}
        </div>

        <div className="min-h-0 flex-1 overflow-y-auto p-4">
          {/* Step 1 — pick repository */}
          {step === 1 && (
            <div className="flex flex-col gap-3">
              <div className="relative">
                <Search className="absolute top-1/2 left-2.5 size-4 -translate-y-1/2 text-muted-foreground" />
                <Input
                  value={repoQuery}
                  onChange={(e) => setRepoQuery(e.target.value)}
                  placeholder="Search connected repositories…"
                  className="pl-8"
                />
              </div>
              <div className="flex flex-col gap-1.5">
                {filteredRepos.map((r) => {
                  const selected = repo?.fullName === r.fullName;
                  return (
                    <button
                      key={r.fullName}
                      type="button"
                      onClick={() => setRepo(r)}
                      className={cn(
                        "flex items-center gap-3 rounded-lg border px-3 py-2.5 text-left transition-colors",
                        selected
                          ? "border-primary bg-primary/5 ring-1 ring-primary/20"
                          : "border-border bg-card hover:bg-muted/50"
                      )}
                    >
                      <GitBranch className="size-4 shrink-0 text-muted-foreground" />
                      <div className="min-w-0 flex-1">
                        <div className="flex items-center gap-2">
                          <span className="truncate font-mono text-sm font-medium text-foreground">
                            {r.fullName}
                          </span>
                          <Badge variant="outline" className="font-mono text-[10px]">
                            {r.private ? "private" : "public"}
                          </Badge>
                        </div>
                        <p className="truncate text-xs text-muted-foreground">
                          {r.description}
                        </p>
                      </div>
                      {selected && <Check className="size-4 shrink-0 text-primary" />}
                    </button>
                  );
                })}
                {filteredRepos.length === 0 && (
                  <p className="py-6 text-center text-sm text-muted-foreground">
                    No repositories match “{repoQuery}”.
                  </p>
                )}
              </div>
            </div>
          )}

          {/* Step 2 — detected build */}
          {step === 2 && repo && (
            <div className="flex flex-col gap-3">
              <div className="rounded-lg border border-border bg-card p-3">
                <div className="flex items-center gap-2">
                  <GitBranch className="size-4 text-muted-foreground" />
                  <span className="font-mono text-sm font-medium text-foreground">
                    {repo.fullName}
                  </span>
                  <Badge variant="outline" className="font-mono text-[10px]">
                    {repo.defaultBranch}
                  </Badge>
                </div>
              </div>

              <div className="flex items-start gap-3 rounded-lg border border-emerald-500/30 bg-emerald-500/5 p-3">
                <CircleCheck className="mt-0.5 size-4 shrink-0 text-emerald-600" />
                <div className="min-w-0">
                  <p className="text-sm font-medium text-foreground">
                    Detected{" "}
                    <span className="font-mono">{repo.build}</span>
                  </p>
                  <p className="text-xs text-muted-foreground">{repo.buildDetail}</p>
                </div>
              </div>

              <dl className="grid grid-cols-2 gap-x-4 gap-y-3 rounded-lg border border-border bg-card p-3 text-sm">
                <div className="flex flex-col gap-0.5">
                  <dt className="text-xs text-muted-foreground">Build method</dt>
                  <dd className="flex items-center gap-1.5 text-foreground">
                    <FileCode2 className="size-3.5 text-muted-foreground" />
                    {repo.build === "Dockerfile" ? "Dockerfile" : "Compose"}
                  </dd>
                </div>
                <div className="flex flex-col gap-0.5">
                  <dt className="text-xs text-muted-foreground">Resource kind</dt>
                  <dd>
                    <Badge variant="outline" className="font-mono">
                      {KIND_LABELS[repo.detectedKind]}
                    </Badge>
                  </dd>
                </div>
                <div className="flex flex-col gap-0.5">
                  <dt className="text-xs text-muted-foreground">Exposed port</dt>
                  <dd className="font-mono text-foreground tabular-nums">{repo.port}</dd>
                </div>
                <div className="flex flex-col gap-0.5">
                  <dt className="text-xs text-muted-foreground">Branch</dt>
                  <dd className="font-mono text-foreground">{repo.defaultBranch}</dd>
                </div>
              </dl>
            </div>
          )}

          {/* Step 3 — target environment + server (availability matrix) */}
          {step === 3 && repo && (
            <div className="flex flex-col gap-4">
              <div className="flex flex-col gap-1.5">
                <Label htmlFor="wizard-project">
                  <Boxes className="size-3.5 text-muted-foreground" />
                  Project
                </Label>
                <Select
                  value={projectId}
                  onValueChange={(v) => {
                    setProjectId(v as string);
                    setEnvironmentId("");
                    setServerId("");
                  }}
                >
                  <SelectTrigger id="wizard-project" className="w-full">
                    <SelectValue placeholder="Select a project">
                      {(v) =>
                        projects.find((p) => p.id === v)?.name ??
                        "Select a project"
                      }
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
                  onValueChange={(v) => {
                    setEnvironmentId(v as string);
                    setServerId("");
                  }}
                  disabled={!projectId}
                >
                  <SelectTrigger id="wizard-env" className="w-full">
                    <SelectValue placeholder="Select an environment">
                      {(v) =>
                        environments.find((e) => e.id === v)?.name ??
                        "Select an environment"
                      }
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
                  Target server
                </Label>
                {!environmentId ? (
                  <p className="rounded-lg border border-dashed border-border px-3 py-4 text-center text-xs text-muted-foreground">
                    Choose an environment to see eligible servers.
                  </p>
                ) : candidateServers.length === 0 ? (
                  <p className="rounded-lg border border-dashed border-border px-3 py-4 text-center text-xs text-muted-foreground">
                    No servers attached to this environment.
                  </p>
                ) : (
                  <div className="flex flex-col gap-1.5">
                    {candidateServers.map(({ server, compatible }) => {
                      const selected = serverId === server.id;
                      return (
                        <button
                          key={server.id}
                          type="button"
                          disabled={!compatible}
                          onClick={() => compatible && setServerId(server.id)}
                          className={cn(
                            "flex items-center gap-3 rounded-lg border px-3 py-2.5 text-left transition-colors",
                            !compatible &&
                              "cursor-not-allowed border-border bg-muted/40 opacity-60",
                            compatible &&
                              (selected
                                ? "border-primary bg-primary/5 ring-1 ring-primary/20"
                                : "border-border bg-card hover:bg-muted/50")
                          )}
                        >
                          <ServerIcon className="size-4 shrink-0 text-muted-foreground" />
                          <div className="min-w-0 flex-1">
                            <div className="flex items-center gap-2">
                              <span className="truncate font-mono text-sm text-foreground">
                                {server.name}
                              </span>
                              <Badge variant="outline" className="text-[10px]">
                                {SERVER_TYPE_LABELS[server.type as ServerType]}
                              </Badge>
                            </div>
                            <p className="truncate text-xs text-muted-foreground">
                              {server.provider} · {server.region}
                            </p>
                          </div>
                          {!compatible ? (
                            <span className="shrink-0 text-[10px] font-medium text-destructive">
                              incompatible
                            </span>
                          ) : (
                            selected && (
                              <Check className="size-4 shrink-0 text-primary" />
                            )
                          )}
                        </button>
                      );
                    })}
                  </div>
                )}

                {kind && (
                  <p className="text-xs text-muted-foreground">
                    Only servers that can host a{" "}
                    <span className="font-medium text-foreground">
                      {KIND_LABELS[kind]}
                    </span>{" "}
                    resource are selectable.
                  </p>
                )}

                {incompatibility && (
                  <div className="flex items-start gap-2 rounded-lg border border-destructive/30 bg-destructive/5 p-3">
                    <CircleAlert className="mt-0.5 size-4 shrink-0 text-destructive" />
                    <div className="min-w-0 text-xs">
                      <p className="font-medium text-destructive">
                        {incompatibility.code} ·{" "}
                        <span className="font-mono">{incompatibility.type}</span>
                      </p>
                      <p className="text-destructive/90">{incompatibility.message}</p>
                    </div>
                  </div>
                )}
              </div>
            </div>
          )}

          {/* Step 4 — environment variables */}
          {step === 4 && (
            <div className="flex flex-col gap-3">
              <p className="text-sm text-muted-foreground">
                Injected into the container at runtime. Values are encrypted at rest.
              </p>
              <div className="flex flex-col gap-2">
                {envVars.map((ev) => (
                  <div key={ev.id} className="flex items-center gap-2">
                    <Input
                      value={ev.key}
                      onChange={(e) =>
                        updateEnvVar(ev.id, { key: e.target.value.toUpperCase() })
                      }
                      placeholder="KEY"
                      className="font-mono"
                    />
                    <Input
                      value={ev.value}
                      onChange={(e) => updateEnvVar(ev.id, { value: e.target.value })}
                      placeholder="value"
                      className="font-mono"
                    />
                    <Button
                      variant="ghost"
                      size="icon-sm"
                      aria-label="Remove variable"
                      onClick={() => removeEnvVar(ev.id)}
                    >
                      <Trash2 className="size-4 text-muted-foreground" />
                    </Button>
                  </div>
                ))}
              </div>
              <Button
                variant="outline"
                size="sm"
                className="w-fit"
                onClick={addEnvVar}
              >
                <Plus className="size-3.5" />
                Add variable
              </Button>
            </div>
          )}

          {/* Step 5 — deploy progress */}
          {step === 5 && repo && (
            <div className="flex flex-col gap-4">
              <div className="flex items-center gap-3">
                <span
                  className={cn(
                    "grid size-9 shrink-0 place-items-center rounded-lg",
                    deployDone
                      ? "bg-emerald-500/10 text-emerald-600"
                      : "bg-primary/10 text-primary"
                  )}
                >
                  {deployDone ? (
                    <CircleCheck className="size-5" />
                  ) : (
                    <Loader2 className="size-5 animate-spin" />
                  )}
                </span>
                <div className="min-w-0">
                  <p className="text-sm font-medium text-foreground">
                    {deployDone ? "Deployment complete" : "Deploying…"}
                  </p>
                  <p className="truncate font-mono text-xs text-muted-foreground">
                    {repo.fullName} → {selectedServer?.name}
                  </p>
                </div>
              </div>

              <Progress
                value={(deployPhase / DEPLOY_PHASES.length) * 100}
                className="w-full"
              />

              <ol className="flex flex-col gap-2">
                {DEPLOY_PHASES.map((phase, i) => {
                  const state =
                    i < deployPhase ? "done" : i === deployPhase ? "active" : "pending";
                  return (
                    <li key={phase} className="flex items-center gap-2.5 text-sm">
                      <span className="grid size-5 shrink-0 place-items-center">
                        {state === "done" && (
                          <CircleCheck className="size-4 text-emerald-600" />
                        )}
                        {state === "active" && (
                          <Loader2 className="size-4 animate-spin text-primary" />
                        )}
                        {state === "pending" && (
                          <span className="size-1.5 rounded-full bg-muted-foreground/30" />
                        )}
                      </span>
                      <span
                        className={cn(
                          state === "pending"
                            ? "text-muted-foreground"
                            : "text-foreground"
                        )}
                      >
                        {phase}
                      </span>
                    </li>
                  );
                })}
              </ol>
            </div>
          )}
        </div>

        {/* Footer / navigation */}
        <div className="flex items-center justify-between gap-2 border-t bg-muted/50 p-4">
          {step < 5 ? (
            <>
              <Button
                variant="ghost"
                size="sm"
                onClick={back}
                disabled={step === 1}
              >
                <ChevronLeft className="size-4" />
                Back
              </Button>
              {step === 4 ? (
                <Button size="sm" onClick={next}>
                  Deploy
                  <ArrowRight className="size-4" />
                </Button>
              ) : (
                <Button size="sm" onClick={next} disabled={!canNext}>
                  Continue
                  <ChevronRight className="size-4" />
                </Button>
              )}
            </>
          ) : (
            <>
              <span className="text-xs text-muted-foreground">
                {deployDone ? "You're all set." : "Please wait…"}
              </span>
              <Button
                size="sm"
                onClick={() => onOpenChange(false)}
                disabled={!deployDone}
              >
                <Check className="size-4" />
                Done
              </Button>
            </>
          )}
        </div>
      </DialogContent>
    </Dialog>
  );
}
