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

import {
  kindsInCategory,
  type ResourceCategoryId,
  type ResourceKind,
} from "@/lib/server-catalog.generated";

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
 *
 * A change of CATEGORY is the same event and needs no rule of its own: it is
 * pickCategory's kind that arrives here, which is the category's only kind or
 * null while its list is open — and null has no steps but the picker, so a
 * half-configured Redis cannot stand on the Build screen by way of a category
 * either.
 */
export function resolveStep(
  kind: ResourceKind | null | undefined,
  id: WizardStepId
): WizardStepId {
  return hasStep(kind, id) ? id : "kind";
}

/**
 * Step 1 asks for a CATEGORY first, and only then — when the category holds
 * more than one kind — for the kind inside it. That is a substate of the "kind"
 * step, not a step of its own, and the choice was not a matter of taste.
 *
 * A "category" step id would appear in every flow's array: a chip in the
 * stepper and a screen in the count, so decisionCount() would read one higher
 * for every kind. SIGMA-212's requirement is a NUMBER and steps.test.ts asserts
 * it, so the sequence must be exactly what it was.
 *
 * The deciding reason is the other one. A category holding exactly one kind is
 * a question with a single possible answer, and pickCategory below answers it
 * instead of asking — so a "step" whose forward move is automatic for three of
 * today's four categories is not a step. It is one screen with two faces, which
 * is what this is.
 */
export type KindPickerPhase = "categories" | "kinds";

/** Step 1's whole state: the category being offered from, and the kind chosen
 *  inside it. Both null before anything is picked. */
export type KindPickerState = {
  category: ResourceCategoryId | null;
  kind: ResourceKind | null;
};

/**
 * Which face step 1 is showing. A category resolved to its only kind never
 * opened a list, so it is still the category grid — with that card selected.
 */
export function kindPickerPhase(
  category: ResourceCategoryId | null | undefined
): KindPickerPhase {
  return category && kindsInCategory(category).length > 1 ? "kinds" : "categories";
}

/**
 * Picking a category — or none, which is what backing out of the kind list
 * does.
 *
 * A category holding exactly ONE kind resolves straight through to it. Asking a
 * question with a single possible answer is the thing this whole rework exists
 * to delete, and re-introducing it on the new screen for the sake of symmetry
 * would be the same defect wearing a category's name. Today only Database holds
 * more than one kind, so today only Database shows a second list; the day
 * Application gains a second kind it starts showing one without an edit here.
 *
 * Leaving a category drops the kind with it: the screen stops showing what was
 * chosen, and a Continue that still advanced on it would be acting on a
 * selection the user can no longer see.
 */
export function pickCategory(id: ResourceCategoryId | null): KindPickerState {
  if (!id) return { category: null, kind: null };
  const kinds = kindsInCategory(id);
  return { category: id, kind: kinds.length === 1 ? kinds[0] : null };
}
