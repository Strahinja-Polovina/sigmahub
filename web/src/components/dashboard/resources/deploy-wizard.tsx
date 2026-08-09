"use client";

import * as React from "react";
import { useRouter } from "next/navigation";
import { toast } from "sonner";
import {
  Rocket,
  Check,
  ChevronRight,
  ChevronLeft,
  Loader2,
  CircleCheck,
  CircleAlert,
  ArrowRight,
  Lock,
} from "lucide-react";

import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogDescription,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
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
import {
  RESOURCE_CATEGORY_CATALOG,
  RESOURCE_KIND_LABELS,
  categoryForKind,
  type ResourceCategoryId,
  type ResourceKind,
} from "@/lib/server-catalog.generated";
import {
  buildInventory,
  kindAvailability,
  targetChoices,
  type WizardCluster,
  type WizardProject,
} from "@/lib/wizard/availability";
import {
  kindPickerPhase,
  nextStepId,
  pickCategory,
  prevStepId,
  resolveStep,
  stepsForKind,
  type WizardStepId,
} from "@/lib/wizard/steps";
import {
  BUILD_COMPOSE,
  decideBuildMethod,
  subdirError,
  type BuildMethod,
  type DetectedRepo,
} from "@/lib/wizard/build";
import {
  createResourceInput,
  createSecretsFor,
  shouldWireRepo,
  type WizardDraftState,
} from "@/lib/wizard/create";
import {
  defaultHealthPath,
  defaultPortMappings,
  domainError,
  portMappingsError,
  type PortMapping,
} from "@/lib/wizard/networking";
import {
  blankEnvDraft,
  envVarCount,
  envVarsValid,
  seedEnvVars,
  type EnvDraft,
} from "@/lib/wizard/env";
import { blockingGaps, type ReviewInput } from "@/lib/wizard/review";
import {
  DEFAULT_LLM_ENGINE,
  DEFAULT_S3_ENGINE,
  S3_ENGINES,
  defaultManagedName,
  isDatabaseKind,
  isManagedKind,
  managedSummary,
  resourceNameError,
} from "@/lib/wizard/managed";
import { modelStepError, serverFitsModel, type ModelCard } from "@/lib/wizard/llm";
import {
  WIZARD_RESUME_KEY,
  decodeWizardDraft,
  encodeWizardDraft,
  type WizardDraft,
} from "@/lib/wizard/resume";
import { findMockRepo, type MockRepo } from "@/lib/mock/repos";
import { createResource } from "@/server/actions/resources";
import { createSecretAction } from "@/server/actions/secrets";
import { detectRepo, getGitAppInfo, wireRepoToEnvironment } from "@/server/actions/git";
import { revealDatabaseConnection } from "@/server/actions/databases";
import { revealS3Connection } from "@/server/actions/s3";
import type { DeployTarget } from "./resource-meta";
import { KindStep } from "./wizard/kind-step";
import { SourceStep } from "./wizard/source-step";
import { ModelStep } from "./wizard/model-step";
import { ResourceNameField } from "./wizard/resource-name-field";
import { BuildStep } from "./wizard/build-step";
import { NetworkStep } from "./wizard/network-step";
import { TargetStep } from "./wizard/target-step";
import { EnvStep } from "./wizard/env-step";
import { ReviewStep } from "./wizard/review-step";

type PickedRepo = {
  fullName: string;
  defaultBranch: string;
  installationId?: string;
  branches?: string[];
};

/** Credentials a managed resource generated, shown once. */
type RevealedCredentials = { label: string; value: string; secret?: boolean }[];

export function DeployWizard({
  open,
  onOpenChange,
  targets,
  cpMode = false,
  orgId = "",
  clusters = [],
  clusterExcludedKinds = [],
  /** The project this wizard was opened from, when it was opened from one. It
   *  brings the GitHub install round trip back to the right page, and it SEEDS
   *  the target project — which is the only reason the model step can ask the
   *  weights-token question about a project at all (see the reset below). */
  originProjectId,
  /** Reopened after a GitHub install — restore the draft the user left. */
  resume = false,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  targets: DeployTarget[];
  cpMode?: boolean;
  orgId?: string;
  clusters?: WizardCluster[];
  clusterExcludedKinds?: string[];
  originProjectId?: string;
  resume?: boolean;
}) {
  const router = useRouter();

  const [kind, setKind] = React.useState<ResourceKind | null>(null);
  /** Step 1's first face. A substate of the "kind" step, not a step — see
   *  wizard/steps.ts for why that is the only shape that keeps a single-kind
   *  category free of a second click. */
  const [category, setCategory] = React.useState<ResourceCategoryId | null>(null);
  const [step, setStep] = React.useState<WizardStepId>("kind");
  const [name, setName] = React.useState("");

  // Source
  const [repo, setRepo] = React.useState<PickedRepo | null>(null);
  const [branch, setBranch] = React.useState("");
  const [manualRepo, setManualRepo] = React.useState("");
  const [token, setToken] = React.useState("");
  const [detecting, setDetecting] = React.useState(false);
  const [detected, setDetected] = React.useState<DetectedRepo | null>(null);
  const [detectError, setDetectError] = React.useState<string | null>(null);
  const [gitAppSlug, setGitAppSlug] = React.useState<string | null>(null);

  // Build
  const [method, setMethod] = React.useState<BuildMethod | null>(null);
  const [dockerfile, setDockerfile] = React.useState("");
  const [contextSubdir, setContextSubdir] = React.useState("");

  // Networking
  const [ports, setPorts] = React.useState<PortMapping[]>([]);
  const [domain, setDomain] = React.useState("");
  const [healthPath, setHealthPath] = React.useState("/");

  // Target
  const [projectId, setProjectId] = React.useState("");
  const [environmentId, setEnvironmentId] = React.useState("");
  const [serverId, setServerId] = React.useState("");
  const [clusterId, setClusterId] = React.useState("");

  // Variables
  const [envVars, setEnvVars] = React.useState<EnvDraft[]>([blankEnvDraft()]);

  // Managed
  const [s3Engine, setS3Engine] = React.useState<string>(DEFAULT_S3_ENGINE);
  const [llmModel, setLlmModel] = React.useState("");
  /** The chosen model's card, when the catalogue could confirm it. Null covers
   *  both "nothing picked yet" and "typed an id the Hub does not know", and the
   *  second is deliberately not an error — see modelStepError. */
  const [modelCard, setModelCard] = React.useState<ModelCard | null>(null);
  /** Whether the control plane holds a Hub token. Published by every search;
   *  held here because it is the model step's continue gate, not the picker's
   *  private business. */
  const [tokenConfigured, setTokenConfigured] = React.useState(false);

  // The runtime is DERIVED, not chosen (SIGMA-213). vLLM cannot load GGUF
  // weights and Ollama does not want a safetensors repository, so the question
  // the old wizard asked had exactly one right answer per model — one the
  // operator had to look up, and got wrong. The card carries the runtime the
  // control plane would render; the default stands in only until a model is
  // resolved, which is also what an unresolvable reference deploys with.
  const llmEngine = modelCard?.engine || DEFAULT_LLM_ENGINE;

  // Create
  const [createState, setCreateState] = React.useState<
    "idle" | "creating" | "done" | "error"
  >("idle");
  const [createdId, setCreatedId] = React.useState<string | null>(null);
  const [createError, setCreateError] = React.useState<string | null>(null);
  const [credentials, setCredentials] = React.useState<RevealedCredentials | null>(null);
  const createStartedRef = React.useRef(false);

  const projects = targets as WizardProject[];
  const inventory = React.useMemo(
    () => buildInventory(projects, clusters, clusterExcludedKinds),
    [projects, clusters, clusterExcludedKinds]
  );
  const steps = stepsForKind(kind);
  const decision = React.useMemo(() => decideBuildMethod(detected), [detected]);

  const environments = React.useMemo(
    () => projects.find((p) => p.id === projectId)?.environments ?? [],
    [projects, projectId]
  );
  const environment = environments.find((e) => e.id === environmentId);
  const server = environments.flatMap((e) => e.servers).find((s) => s.id === serverId);
  const cluster = clusters.find((c) => c.id === clusterId);
  const project = projects.find((p) => p.id === projectId);

  // Built here rather than inside the step because the Continue gate reads the
  // same verdict the step renders: when every target in the environment is
  // refused, the button's reason has to be the refusal's own sentence and not
  // "Pick a server or a cluster", which is advice for a screen where something
  // is pickable.
  const choices = React.useMemo(
    () =>
      targetChoices({
        projects,
        clusters,
        inventory,
        kind,
        model: modelCard,
        projectId,
        environmentId,
      }),
    [projects, clusters, inventory, kind, modelCard, projectId, environmentId]
  );

  // Memoized because the step guard below depends on it: a fresh object on
  // every render would recompute (and re-render) on every keystroke.
  const reviewInput: ReviewInput = React.useMemo(
    () => ({
      kind: kind ?? "app",
      name,
      repo: repo?.fullName,
      branch,
      buildMethod: kind === "app" ? method : null,
      dockerfile,
      contextSubdir,
      composeServiceCount: detected?.services?.length,
      ports: kind === "app" && method !== BUILD_COMPOSE ? ports : undefined,
      domain,
      healthPath: kind === "app" && method !== BUILD_COMPOSE ? healthPath : undefined,
      envVarCount: kind === "app" ? envVarCount(envVars) : undefined,
      llmEngine: kind === "llm" ? llmEngine : undefined,
      llmModel: kind === "llm" ? llmModel : undefined,
      engineVersion: kind === "s3" ? s3Engine : undefined,
      projectName: project?.name,
      environmentName: environment?.name,
      serverName: server?.name,
      clusterName: cluster?.name,
    }),
    [
      kind,
      name,
      repo,
      branch,
      method,
      dockerfile,
      contextSubdir,
      detected,
      ports,
      domain,
      healthPath,
      envVars,
      llmEngine,
      llmModel,
      s3Engine,
      project,
      environment,
      server,
      cluster,
    ]
  );

  // ── Draft persistence across the GitHub install round trip ────────────────

  const stashDraft = React.useCallback(() => {
    if (!kind) return;
    const draft: WizardDraft = {
      kind,
      ...(name.trim() ? { name: name.trim() } : {}),
      ...(projectId ? { projectId } : {}),
      ...(environmentId ? { environmentId } : {}),
      ...(serverId ? { serverId } : {}),
      ...(clusterId ? { clusterId } : {}),
      ...(repo?.fullName ? { repo: repo.fullName } : {}),
      ...(branch ? { branch } : {}),
    };
    try {
      window.sessionStorage.setItem(WIZARD_RESUME_KEY, encodeWizardDraft(draft));
    } catch {
      // A blocked sessionStorage costs the user their draft, not the flow.
    }
  }, [kind, name, projectId, environmentId, serverId, clusterId, repo, branch]);

  // Reset (or restore) whenever the dialog opens. Adjusting during the render
  // that flips `open` avoids a setState-in-effect and the extra committed
  // render it would cause.
  //
  // Seeded FALSE, not from `open`. Both resume call sites mount this component
  // already open — the whole point is to come back from github.com straight
  // into the wizard — and `useState(open)` made prevOpen match on the very
  // first render, so the restore below could not fire on the one render that
  // matters. The user returned to an empty wizard on step 1, which is exactly
  // the failure the draft exists to prevent; worse, the draft was never
  // cleared, so closing and reopening later resurrected it.
  const [prevOpen, setPrevOpen] = React.useState(false);
  if (open !== prevOpen) {
    setPrevOpen(open);
    if (open) {
      let draft: WizardDraft | null = null;
      if (resume) {
        try {
          draft = decodeWizardDraft(window.sessionStorage.getItem(WIZARD_RESUME_KEY));
          window.sessionStorage.removeItem(WIZARD_RESUME_KEY);
        } catch {
          draft = null;
        }
      }
      setKind(draft?.kind ?? null);
      // The draft carries the KIND, because that is what the rest of the wizard
      // is decided by; the category it sits in is derived rather than stored, so
      // a draft written before categories existed still reopens on the right
      // screen instead of on a picker showing nothing selected.
      setCategory(draft ? categoryForKind(draft.kind) : null);
      // A restored draft lands on Source, which is where the user was when they
      // left for github.com — dropping them back on step 1 would technically
      // preserve their state and still feel like starting over.
      setStep(draft ? resolveStep(draft.kind, "source") : "kind");
      setName(draft?.name ?? "");
      setRepo(null);
      setBranch(draft?.branch ?? "");
      setManualRepo(draft?.repo ?? "");
      setToken("");
      setDetected(null);
      setDetectError(null);
      setDetecting(false);
      setMethod(null);
      setDockerfile("");
      setContextSubdir("");
      setPorts([]);
      setDomain("");
      setHealthPath("/");
      // Seeded from the project this wizard was opened FROM, when there was
      // one. The step order is kind → model → target, so the project is
      // otherwise unknown while the model is being picked — and the model
      // step's weights-token question is asked per project. Without this seed
      // it was always asked with no project, which collapsed the answer to "is
      // a token set on the control plane" and told an operator whose token
      // lives in their project's secrets that none was configured, on every
      // gated model. It is also simply the right default for a wizard opened
      // inside a project: the target step lands pre-filled, and the user may
      // still change it.
      setProjectId(draft?.projectId ?? originProjectId ?? "");
      setEnvironmentId(draft?.environmentId ?? "");
      setServerId(draft?.serverId ?? "");
      setClusterId(draft?.clusterId ?? "");
      setEnvVars([blankEnvDraft()]);
      setS3Engine(DEFAULT_S3_ENGINE);
      setLlmModel("");
      setModelCard(null);
      setTokenConfigured(false);
      setCreateState("idle");
      setCreatedId(null);
      setCreateError(null);
      setCredentials(null);
    }
  }

  React.useEffect(() => {
    if (open) createStartedRef.current = false;
  }, [open]);

  // The App slug drives the inline Connect GitHub link. Loaded once per open,
  // and a failure simply means the offer is not shown.
  React.useEffect(() => {
    if (!open || !cpMode || !orgId) return;
    let cancelled = false;
    getGitAppInfo(orgId)
      .then((info) => {
        if (!cancelled) setGitAppSlug(info.enabled && info.slug ? info.slug : null);
      })
      .catch(() => {
        if (!cancelled) setGitAppSlug(null);
      });
    return () => {
      cancelled = true;
    };
  }, [open, cpMode, orgId]);

  // ── Detection ─────────────────────────────────────────────────────────────

  /** Apply a detection result to the build/network/variable defaults. */
  const applyDetection = React.useCallback((d: DetectedRepo) => {
    setDetected(d);
    const decided = decideBuildMethod(d);
    setMethod(decided.method);
    setDockerfile(d.dockerfilePath ?? "");
    setContextSubdir(d.contextSubdir ?? "");
    const mappings = defaultPortMappings(d.ports);
    setPorts(mappings);
    setHealthPath(defaultHealthPath(d.healthCheck));
    // Keys the repository itself names — the operator fills in values instead
    // of retyping names, and a missed one is a container that dies on start.
    setEnvVars(seedEnvVars(d.env));
  }, []);

  const runDetect = React.useCallback(
    async (fullName: string, installationId?: string) => {
      if (!fullName.includes("/")) {
        setDetectError("Enter the repository as owner/name.");
        return;
      }
      setDetecting(true);
      setDetectError(null);
      try {
        const d = await detectRepo({
          orgId,
          repoFullName: fullName,
          installationId,
          token: token.trim() || undefined,
        });
        if (!d.deployable && !d.hasDockerfile && !d.hasCompose && !d.buildMethod) {
          // NOT a dead end any more: the build step offers a starter Dockerfile
          // and the auto-build fallback, so the repo is still selected.
          setDetectError(d.reason ?? "Couldn't work out how to build this repository.");
        }
        setRepo({
          fullName,
          defaultBranch: d.defaultBranch || "main",
          installationId,
        });
        setBranch((b) => b || d.defaultBranch || "main");
        applyDetection(d as DetectedRepo);
      } catch (err) {
        setDetectError(
          err instanceof Error ? err.message : "Couldn't read the repository."
        );
      } finally {
        setDetecting(false);
      }
    },
    [orgId, token, applyDetection]
  );

  function pickRepo(picked: PickedRepo & { mock?: MockRepo }) {
    setRepo(picked);
    setBranch(picked.defaultBranch);
    setDetectError(null);
    if (!cpMode) {
      // Demo mode has no control plane to read a repository; the fixture IS the
      // inspector's output, so every path stays walkable offline.
      const mock = picked.mock ?? findMockRepo(picked.fullName);
      if (mock) applyDetection(mock.detected);
      return;
    }
    void runDetect(picked.fullName, picked.installationId);
  }

  // ── Navigation ────────────────────────────────────────────────────────────

  const stepBlocked = React.useMemo((): string | null => {
    switch (step) {
      case "kind":
        if (!kind) {
          // Named, because "pick what you're deploying" reads as unanswered on
          // a screen where the category already was.
          return category
            ? `Pick which ${RESOURCE_CATEGORY_CATALOG[category].label.toLowerCase()} to deploy.`
            : "Pick what you're deploying.";
        }
        if (!kindAvailability(kind, inventory).available) {
          return "Nothing in this organization can host that yet.";
        }
        return resourceNameError(name);
      case "source":
        if (!repo) return "Pick a repository.";
        if (!branch.trim()) return "Pick a branch.";
        return null;
      case "build":
        if (!method) return "Choose how this repository gets built.";
        if (subdirError(contextSubdir)) return subdirError(contextSubdir);
        return null;
      case "networking":
        // Both, not just the ports. domainError was rendered in destructive red
        // next to an ENABLED Continue button — a validator with no gate, which
        // let a malformed hostname through to a create that then refused it.
        return portMappingsError(ports) ?? domainError(domain);
      case "engine":
      case "storage":
        return resourceNameError(name);
      case "model":
        if (resourceNameError(name)) return resourceNameError(name);
        return modelStepError(modelCard, llmModel);
      case "target":
        if (!projectId) return "Pick a project.";
        if (!environmentId) return "Pick an environment.";
        if (!serverId && !clusterId) return choices.deadEnd ?? "Pick a server or a cluster.";
        return null;
      case "env":
        return envVarsValid(envVars) ? null : "Fix the variable names first.";
      case "review":
        return blockingGaps(reviewInput)[0] ?? null;
      default:
        return null;
    }
  }, [
    step,
    kind,
    category,
    name,
    inventory,
    repo,
    branch,
    method,
    contextSubdir,
    ports,
    domain,
    llmModel,
    modelCard,
    projectId,
    environmentId,
    serverId,
    clusterId,
    choices,
    envVars,
    reviewInput,
  ]);

  /** A picked or typed model, and the consequences of changing it.
   *
   *  Changing the model after a target was chosen can invalidate that target —
   *  a 70B model on the 24 GB card that was fine for the 8B one. The target
   *  picker will refuse it with the reason, but a serverId already in state
   *  would sail past the Target step's own gate and be refused by the create
   *  call instead, which is the late failure this whole flow exists to remove.
   *  So the selection is dropped here, where the fact that made it invalid is.
   *
   *  A CLUSTER needs no such check: a model endpoint cannot be aimed at one at
   *  all (`llm` is on the control plane's cluster exclusion list), so there is
   *  no clusterId a model change could invalidate. */
  const handleModelChange = React.useCallback(
    (id: string, next: ModelCard | null) => {
      setLlmModel(id);
      setModelCard(next);
      if (serverId) {
        const chosen = projects
          .flatMap((p) => p.environments)
          .flatMap((e) => e.servers)
          .find((s) => s.id === serverId);
        if (chosen && !serverFitsModel(next, chosen).fits) setServerId("");
      }
    },
    [projects, serverId]
  );

  // Step 1 has an inside, so Back does too: from a category's kind list it
  // returns to the categories rather than being the dead button it is on the
  // first screen. Everywhere else it is still simply the previous step.
  const backToCategories = step === "kind" && kindPickerPhase(category) === "kinds";
  const backStep = backToCategories ? null : prevStepId(kind, step);

  function goNext() {
    if (stepBlocked) return;
    const next = nextStepId(kind, step);
    if (next) setStep(next);
  }
  function goBack() {
    if (backToCategories) {
      chooseCategory(null);
      return;
    }
    if (backStep) setStep(backStep);
  }

  /** Step 1's first answer.
   *
   *  A category holding exactly one kind IS that kind (pickCategory), so this
   *  and pickKind must leave identical state — otherwise the flow would depend
   *  on which of step 1's two faces the user happened to answer on. Hence the
   *  delegation rather than a second copy of the same resets.
   *
   *  Passing null is backing out of a kind list, which un-picks the kind with
   *  the category: the screen stops showing what was chosen, and Continue must
   *  not advance on a selection nobody can see. */
  function chooseCategory(next: ResourceCategoryId | null) {
    const picked = pickCategory(next);
    setCategory(picked.category);
    if (picked.kind) {
      pickKind(picked.kind);
      return;
    }
    setKind(null);
    setStep((s) => resolveStep(null, s));
    setServerId("");
    setClusterId("");
  }

  function pickKind(next: ResourceKind) {
    setKind(next);
    // Any step the new kind does not have resolves back to the picker, so a
    // Redis can never be standing on the application flow's Build screen.
    setStep((s) => resolveStep(next, s));
    setName((current) => {
      if (current.trim()) return current;
      return next === "app" ? "" : defaultManagedName(next, environment?.name);
    });
    if (next !== "app") {
      setRepo(null);
      setDetected(null);
      setMethod(null);
    }
    // A kind change can invalidate the chosen server (the matrix differs), so
    // drop it rather than carry an impossible target into the create call.
    setServerId("");
    setClusterId("");
  }

  // ── Create ────────────────────────────────────────────────────────────────

  React.useEffect(() => {
    if (step !== "create" || !kind) return;
    if (createStartedRef.current) return;
    createStartedRef.current = true;
    setCreateState("creating");

    // The whole request, decided in one testable place. Assembling it inline
    // here is what let the Compose graph, the cluster id and the detected ports
    // each be dropped by a single statement with every suite still green.
    const draft: WizardDraftState = {
      kind,
      name,
      projectId,
      environmentId,
      serverId,
      clusterId,
      domain,
      repo,
      branch,
      detected,
      method,
      dockerfile,
      contextSubdir,
      ports,
      healthPath,
      s3Engine,
      llmEngine,
      llmModel,
      envVars,
    };

    (async () => {
      try {
        const { id } = await createResource(createResourceInput(draft));

        // Push-to-deploy for the golden path, in one resilient call. Failures
        // never undo the created resource — they surface as warnings.
        if (shouldWireRepo(draft, cpMode) && repo) {
          const wired = await wireRepoToEnvironment({
            orgId,
            projectId,
            repoFullName: repo.fullName,
            token: token.trim() || undefined,
            installationId: repo.installationId,
            branch: branch.trim() || repo.defaultBranch,
            environmentId,
          });
          if (!wired.ok) {
            toast.warning("Repository not fully wired", {
              description: `${wired.error} — finish the setup in the project's Git panel.`,
            });
          } else if (!wired.webhookRegistered) {
            toast.info("Push webhook not registered", {
              description:
                "Pushes won't auto-deploy yet — add the webhook on GitHub, or reconnect with a token that can manage webhooks.",
            });
          }
        }

        // Variables become secrets AFTER the resource exists. A secret failure
        // must not be reported as a failed create (SIGMA-151), so collect
        // per-key failures and keep going.
        const failed: string[] = [];
        for (const ev of createSecretsFor(draft)) {
          try {
            await createSecretAction({
              resourceId: id,
              name: ev.key,
              value: ev.value,
              scope: "environment",
              envVar: true,
            });
          } catch {
            failed.push(ev.key);
          }
        }

        setCreatedId(id);
        setCreateState("done");
        router.refresh();

        if (failed.length > 0) {
          toast.warning(`${name} created — some variables need attention`, {
            description: `Couldn't save ${failed.length} variable(s): ${failed.join(", ")}. Add them from the resource's Secrets panel.`,
          });
        }

        // Generated credentials, shown once (SIGMA-212). The reveal is audited
        // on both sides; showing them here is the difference between "your
        // database exists" and "your database is usable".
        // Both modes since SIGMA-215. The demo branches of these two actions
        // derive the engine's details from the resource, so the last screen of
        // the managed-data flow stops being blank for five of the seven kinds —
        // and "your database is usable" is the half of SIGMA-212 that a demo
        // user could not see at all.
        if (isManagedKind(kind)) {
          try {
            const creds = isDatabaseKind(kind)
              ? await revealDatabaseConnection({ orgId, resourceId: id }).then((c) => [
                  { label: "Host", value: `${c.host}:${c.port}` },
                  { label: "Database", value: c.database ?? "" },
                  { label: "User", value: c.username ?? "" },
                  {
                    label: "Password",
                    value: c.password ?? "",
                    secret: true,
                  },
                  { label: "URL", value: c.url ?? "", secret: true },
                ])
              : await revealS3Connection({ orgId, resourceId: id }).then((c) => [
                  { label: "Endpoint", value: c.endpoint ?? "" },
                  { label: "Access key", value: c.accessKey ?? "" },
                  {
                    label: "Secret key",
                    value: c.secretKey ?? "",
                    secret: true,
                  },
                ]);
            setCredentials(creds.filter((c) => c.value));
          } catch {
            // The engine may still be provisioning. The panel on the resource
            // page can reveal them whenever it is up — that is not an error.
            setCredentials(null);
          }
        }
      } catch (err) {
        setCreateState("error");
        setCreateError(err instanceof Error ? err.message : "Please try again.");
      }
    })();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [step]);

  const done = createState === "done" || createState === "error";
  const isLast = step === "create";
  const nextLabel = nextStepId(kind, step) === "create" ? "Deploy" : "Continue";

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        className="flex max-h-[85vh] flex-col gap-0 overflow-hidden p-0 sm:max-w-xl"
        showCloseButton={!isLast || done}
      >
        <DialogHeader className="gap-1 border-b p-4">
          <DialogTitle className="flex items-center gap-2">
            <Rocket className="size-4" />
            New resource
          </DialogTitle>
          <DialogDescription>
            {kind
              ? `Deploying ${RESOURCE_KIND_LABELS[kind]}.`
              : "Pick what you're deploying — the rest of this wizard follows from it."}
          </DialogDescription>
        </DialogHeader>

        {/* Stepper. The sequence is the KIND's, so a managed engine never shows
            chips for screens it will not walk. */}
        <div className="flex items-center gap-1 border-b bg-muted/40 px-4 py-2.5">
          {steps.map((s, i) => {
            const at = steps.findIndex((x) => x.id === step);
            const active = s.id === step;
            const complete = i < at;
            return (
              <React.Fragment key={s.id}>
                <div className="flex items-center gap-1.5">
                  <span
                    className={cn(
                      "grid size-5 shrink-0 place-items-center rounded-full border text-[10px] font-semibold tabular-nums",
                      active && "border-primary bg-primary text-primary-foreground",
                      complete && "border-primary/30 bg-primary/10 text-primary",
                      !active &&
                        !complete &&
                        "border-border bg-background text-muted-foreground"
                    )}
                  >
                    {complete ? <Check className="size-3" /> : i + 1}
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
                {i < steps.length - 1 && (
                  <span className="mx-0.5 h-px flex-1 bg-border" aria-hidden />
                )}
              </React.Fragment>
            );
          })}
        </div>

        <div className="min-h-0 flex-1 overflow-y-auto p-4">
          {step === "kind" && (
            <KindStep
              category={category}
              kind={kind}
              onPickCategory={chooseCategory}
              onPickKind={pickKind}
              inventory={inventory}
              name={name}
              onNameChange={setName}
            />
          )}

          {step === "source" && (
            <SourceStep
              cpMode={cpMode}
              orgId={orgId}
              repo={repo}
              branch={branch}
              onPickRepo={pickRepo}
              onBranchChange={setBranch}
              detecting={detecting}
              gitAppSlug={gitAppSlug}
              installUrlTarget={{ kind: "wizard", projectId: originProjectId }}
              onBeforeLeaveForGitHub={stashDraft}
              manualRepo={manualRepo}
              onManualRepoChange={setManualRepo}
              token={token}
              onTokenChange={setToken}
              onDetectManual={() => void runDetect(manualRepo.trim())}
              detectError={detectError}
            />
          )}

          {step === "build" && (
            <BuildStep
              detected={detected}
              decision={decision}
              method={method}
              onMethodChange={setMethod}
              dockerfile={dockerfile}
              onDockerfileChange={setDockerfile}
              contextSubdir={contextSubdir}
              onContextSubdirChange={setContextSubdir}
            />
          )}

          {step === "networking" && (
            <NetworkStep
              ports={ports}
              onPortsChange={setPorts}
              domain={domain}
              onDomainChange={setDomain}
              healthPath={healthPath}
              onHealthPathChange={setHealthPath}
              composeMode={method === BUILD_COMPOSE}
            />
          )}

          {(step === "engine" || step === "storage") && kind && (
            <ManagedStep
              kind={kind}
              name={name}
              onNameChange={setName}
              s3Engine={s3Engine}
              onS3EngineChange={setS3Engine}
            />
          )}

          {step === "model" && (
            <ModelStep
              orgId={orgId}
              projectId={projectId}
              name={name}
              onNameChange={setName}
              modelId={llmModel}
              card={modelCard}
              onModelChange={handleModelChange}
              tokenConfigured={tokenConfigured}
              onTokenConfiguredChange={setTokenConfigured}
            />
          )}

          {step === "target" && kind && (
            <TargetStep
              projects={projects}
              choices={choices}
              projectId={projectId}
              environmentId={environmentId}
              serverId={serverId}
              clusterId={clusterId}
              onProjectChange={(id) => {
                setProjectId(id);
                setEnvironmentId("");
                setServerId("");
                setClusterId("");
              }}
              onEnvironmentChange={(id) => {
                setEnvironmentId(id);
                setServerId("");
                setClusterId("");
              }}
              onServerChange={(id) => {
                setServerId(id);
                setClusterId("");
              }}
              onClusterChange={(id) => {
                setClusterId(id);
                setServerId("");
              }}
            />
          )}

          {step === "env" && (
            <EnvStep
              vars={envVars}
              onChange={setEnvVars}
              seededFromRepo={Boolean(detected?.env?.length)}
            />
          )}

          {step === "review" && <ReviewStep input={reviewInput} onJump={setStep} />}

          {step === "create" && (
            <CreateStep
              state={createState}
              error={createError}
              kind={kind}
              name={name}
              targetName={cluster?.name ?? server?.name ?? ""}
              credentials={credentials}
              createdId={createdId}
              onOpenResource={(id) => {
                onOpenChange(false);
                router.push(`/dashboard/resources/${id}`);
              }}
            />
          )}
        </div>

        <div className="flex items-center justify-between gap-3 border-t bg-muted/50 p-4">
          {!isLast ? (
            <>
              <Button
                variant="ghost"
                size="sm"
                onClick={goBack}
                disabled={!backToCategories && !backStep}
              >
                <ChevronLeft className="size-4" />
                Back
              </Button>
              <div className="flex min-w-0 items-center gap-3">
                {/* Disabled-with-reason, consistently: a greyed-out button whose
                    cause is invisible is the pattern this rebuild removes. */}
                {stepBlocked && (
                  <span className="truncate text-right text-xs text-muted-foreground">
                    {stepBlocked}
                  </span>
                )}
                <Button size="sm" onClick={goNext} disabled={Boolean(stepBlocked)}>
                  {nextLabel}
                  {nextLabel === "Deploy" ? (
                    <ArrowRight className="size-4" />
                  ) : (
                    <ChevronRight className="size-4" />
                  )}
                </Button>
              </div>
            </>
          ) : (
            <>
              <span className="text-xs text-muted-foreground">
                {done ? "You're all set." : "Creating…"}
              </span>
              <Button size="sm" onClick={() => onOpenChange(false)} disabled={!done}>
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

/** The managed path's single configuration step (SIGMA-212).
 *
 *  `llm` used to be here too and now has its own step: picking a model is a
 *  search against the Hub, and the runtime it used to ask for is derived from
 *  what comes back (SIGMA-213). See wizard/model-step.tsx. */
function ManagedStep({
  kind,
  name,
  onNameChange,
  s3Engine,
  onS3EngineChange,
}: {
  kind: ResourceKind;
  name: string;
  onNameChange: (v: string) => void;
  s3Engine: string;
  onS3EngineChange: (v: string) => void;
}) {
  const summary = managedSummary(kind);

  return (
    <div className="flex flex-col gap-4">
      <ResourceNameField
        id="wizard-managed-name"
        value={name}
        onChange={onNameChange}
        hint="Other resources reach it by this name on the private network."
      />

      {kind === "s3" && (
        <div className="flex flex-col gap-1.5">
          <Label htmlFor="wizard-s3-engine">Engine</Label>
          <Select
            value={s3Engine}
            onValueChange={(v) => onS3EngineChange((v as string) ?? "")}
          >
            <SelectTrigger id="wizard-s3-engine" className="w-full">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {S3_ENGINES.map((e) => (
                <SelectItem key={e.id} value={e.id}>
                  {e.label} · {e.detail}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
      )}

      <div className="flex flex-col gap-2 rounded-lg border border-border bg-muted/40 p-3">
        <p className="text-xs text-muted-foreground">{summary.line}</p>
        <p className="flex items-start gap-2 text-xs text-muted-foreground">
          <Lock className="mt-0.5 size-3.5 shrink-0" />
          {summary.credentials}
        </p>
      </div>
    </div>
  );
}

/** The terminal screen: the create call, its outcome, and — for a managed
 *  engine — the credentials it generated, shown once. */
function CreateStep({
  state,
  error,
  kind,
  name,
  targetName,
  credentials,
  createdId,
  onOpenResource,
}: {
  state: "idle" | "creating" | "done" | "error";
  error: string | null;
  kind: ResourceKind | null;
  name: string;
  targetName: string;
  credentials: RevealedCredentials | null;
  createdId: string | null;
  onOpenResource: (id: string) => void;
}) {
  return (
    <div className="flex flex-col gap-4">
      <div className="flex items-center gap-3">
        <span
          className={cn(
            "grid size-9 shrink-0 place-items-center rounded-lg",
            state === "done"
              ? "bg-emerald-500/10 text-emerald-600"
              : state === "error"
                ? "bg-destructive/10 text-destructive"
                : "bg-primary/10 text-primary"
          )}
        >
          {state === "done" ? (
            <CircleCheck className="size-5" />
          ) : state === "error" ? (
            <CircleAlert className="size-5" />
          ) : (
            <Loader2 className="size-5 animate-spin" />
          )}
        </span>
        <div className="min-w-0">
          <p className="text-sm font-medium text-foreground">
            {state === "done"
              ? "Resource created"
              : state === "error"
                ? "Create failed"
                : "Creating resource…"}
          </p>
          <p className="truncate font-mono text-xs text-muted-foreground">
            {name}
            {targetName ? ` → ${targetName}` : ""}
          </p>
        </div>
      </div>

      {state === "error" && error && (
        <p className="rounded-lg border border-destructive/30 bg-destructive/5 p-3 text-sm text-destructive/90">
          {error}
        </p>
      )}

      {state === "done" && (
        <p className="rounded-lg border border-border bg-muted/40 p-3 text-sm text-muted-foreground">
          {kind === "app" ? (
            <>
              It starts as <span className="font-medium">provisioning</span> until its first
              build finishes. The branch you picked is mapped to this environment, so
              pushing to it rolls out a new version.
            </>
          ) : (
            <>
              It starts as <span className="font-medium">provisioning</span>. The
              server&apos;s agent pulls the image, starts it, and reports it running.
            </>
          )}
        </p>
      )}

      {credentials && credentials.length > 0 && (
        <div className="flex flex-col gap-2 rounded-lg border border-amber-500/30 bg-amber-500/5 p-3">
          <p className="flex items-center gap-2 text-sm font-medium text-foreground">
            <Lock className="size-3.5" />
            Generated credentials
          </p>
          <p className="text-xs text-muted-foreground">
            Copy what you need now. After this screen they are behind an audited reveal on
            the resource page.
          </p>
          <dl className="flex flex-col gap-1.5">
            {credentials.map((c) => (
              <div key={c.label} className="flex items-baseline gap-2">
                <dt className="w-24 shrink-0 text-xs text-muted-foreground">{c.label}</dt>
                <dd className="min-w-0 flex-1 font-mono text-xs break-all text-foreground">
                  {c.value}
                </dd>
                {c.secret && (
                  <Badge variant="outline" className="shrink-0 text-[10px]">
                    secret
                  </Badge>
                )}
              </div>
            ))}
          </dl>
        </div>
      )}

      {state === "done" && createdId && (
        <Button
          variant="outline"
          size="sm"
          className="w-fit"
          onClick={() => onOpenResource(createdId)}
        >
          Open resource
          <ArrowRight className="size-4" />
        </Button>
      )}
    </div>
  );
}
