/**
 * The New Resource wizard's step sequence, per resource type (SIGMA-207).
 *
 * The wizard used to run ONE sequence for five very different things. A managed
 * Redis needs two decisions — which engine, which server — and walked five
 * screens to make them, two of which (Source, Build) were about a git
 * repository it does not have. An application needs six and got the same five,
 * so ports, domains, health checks and the choice between a server and a cluster
 * had nowhere to live and simply were not asked.
 *
 * The fix is that the type picked on step 1 decides what the rest of the wizard
 * IS. That decision lives here, as data, so "how many screens does a Redis
 * walk" is a question a test can answer.
 */

import type { ResourceKind } from "@/lib/server-catalog.generated";

export type WizardStepId =
  | "kind"
  | "source"
  | "build"
  | "networking"
  | "target"
  | "env"
  | "review"
  | "engine"
  | "storage"
  | "model"
  | "create";

export type WizardStep = {
  id: WizardStepId;
  /** Shown in the stepper. Short: eight of these share a dialog header. */
  label: string;
};

/** Step 1 is the same for everything: pick what you are deploying. */
const KIND: WizardStep = { id: "kind", label: "Type" };

/** The terminal step. Not a decision — it is the create call and its progress. */
const CREATE: WizardStep = { id: "create", label: "Deploy" };

const APP_STEPS: WizardStep[] = [
  KIND,
  { id: "source", label: "Source" },
  { id: "build", label: "Build" },
  { id: "networking", label: "Network" },
  { id: "target", label: "Target" },
  { id: "env", label: "Variables" },
  { id: "review", label: "Review" },
  CREATE,
];

// A managed engine is provisioned from its official image. There is no repo to
// pick, nothing to build, no ports to map (the engine's port is the engine's),
// and no variables to set (the credentials are generated). Two decisions:
// which version, and which server.
const DATABASE_STEPS: WizardStep[] = [
  KIND,
  { id: "engine", label: "Version" },
  { id: "target", label: "Target" },
  CREATE,
];

const STORAGE_STEPS: WizardStep[] = [
  KIND,
  { id: "storage", label: "Bucket" },
  { id: "target", label: "Target" },
  CREATE,
];

const LLM_STEPS: WizardStep[] = [
  KIND,
  { id: "model", label: "Model" },
  { id: "target", label: "Target" },
  CREATE,
];

const BY_KIND: Record<ResourceKind, WizardStep[]> = {
  app: APP_STEPS,
  postgres: DATABASE_STEPS,
  mysql: DATABASE_STEPS,
  mongodb: DATABASE_STEPS,
  redis: DATABASE_STEPS,
  s3: STORAGE_STEPS,
  llm: LLM_STEPS,
};

/**
 * The steps this kind walks. Before a kind is picked there is exactly one step,
 * because there is exactly one thing to do — and showing an application's eight
 * chips to someone who might be about to pick Redis is the old wizard's
 * promise, made before it knows what it is promising.
 */
export function stepsForKind(kind: ResourceKind | null | undefined): WizardStep[] {
  if (!kind) return [KIND];
  return BY_KIND[kind] ?? APP_STEPS;
}

/** Whether a kind's flow includes a step — the guard every step body needs. */
export function hasStep(kind: ResourceKind | null | undefined, id: WizardStepId): boolean {
  return stepsForKind(kind).some((s) => s.id === id);
}

/**
 * How many DECISIONS a kind asks for beyond picking the type. Review and the
 * create screen are not decisions; they are a summary and a progress bar.
 *
 * This exists to be asserted: SIGMA-212's requirement is a number ("two
 * decisions maximum beyond the kind"), and a requirement stated as a number
 * that nothing counts is a requirement that drifts.
 */
export function decisionCount(kind: ResourceKind | null | undefined): number {
  return stepsForKind(kind).filter(
    (s) => s.id !== "kind" && s.id !== "create" && s.id !== "review"
  ).length;
}

/** Position of a step in its kind's flow, or -1. */
export function stepIndex(kind: ResourceKind | null | undefined, id: WizardStepId): number {
  return stepsForKind(kind).findIndex((s) => s.id === id);
}

/**
 * The step after `id`, or null at the end.
 *
 * Movement is expressed in step IDS rather than in numbers because the flows
 * have different lengths: the old wizard tracked a number and had to special-
 * case "if this is a managed service, 1 goes to 3", which is how a managed
 * service came to be able to reach the Build screen by pressing Back.
 */
export function nextStepId(
  kind: ResourceKind | null | undefined,
  id: WizardStepId
): WizardStepId | null {
  const steps = stepsForKind(kind);
  const i = steps.findIndex((s) => s.id === id);
  if (i < 0 || i + 1 >= steps.length) return null;
  return steps[i + 1].id;
}

/** The step before `id`, or null at the start. */
export function prevStepId(
  kind: ResourceKind | null | undefined,
  id: WizardStepId
): WizardStepId | null {
  const steps = stepsForKind(kind);
  const i = steps.findIndex((s) => s.id === id);
  if (i <= 0) return null;
  return steps[i - 1].id;
}

/**
 * Where a change of kind lands you.
 *
 * Picking a different type on step 1 while standing on step 4 used to leave the
 * step number alone, so a Redis could be standing on the application flow's
 * "Build" screen — which renders nothing for it, because it has no repo. Any
 * step the new kind does not have resolves back to the type picker.
 */
export function resolveStep(
  kind: ResourceKind | null | undefined,
  id: WizardStepId
): WizardStepId {
  return hasStep(kind, id) ? id : "kind";
}
