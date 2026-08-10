/**
 * Whether a resource kind can be deployed at all, answered on STEP ONE
 * (SIGMA-207).
 *
 * The old wizard let you pick "Object storage", name it, walk to the target
 * screen and only there discover that the organization has no Storage server —
 * four decisions spent on a resource that was never creatable. The LLM path was
 * worse: it asked for a runtime and a model reference before mentioning that
 * nothing here has a GPU.
 *
 * A card that cannot lead anywhere has to say so where it is offered, with the
 * one thing that would fix it. That is what this module decides; the wizard only
 * renders it. "Where it is offered" is now two places — a category card and the
 * kind cards inside it — so the verdict rolls up as well as down.
 */

import {
  ALLOWED_SERVER_TYPES,
  RESOURCE_CATEGORY_CATALOG,
  RESOURCE_KIND_LABELS,
  SERVER_TYPE_LABELS,
  kindsInCategory,
  type ResourceCategoryId,
  type ResourceKind,
  type ServerType,
} from "@/lib/server-catalog.generated";
import { SERVER_STATUS, type HostFacts } from "@/lib/server-compat";
import { serverFitsModel, vramNeedText, type ModelCard } from "./llm";

/** A server as the wizard sees it — the deploy-target shape, plus the status
 *  the enrollment gate wrote. */
export type WizardServer = {
  id: string;
  name: string;
  type: string;
  provider?: string;
  region?: string;
  status?: string;
  /** The host's GPU inventory as the agent reported it (SIGMA-201), and only
   *  that slice of its facts: it is the one thing a target decision needs that
   *  the type alone cannot answer. Absent means no agent ever reported one,
   *  which is UNKNOWN and never zero — see serverFitsModel. */
  gpu?: HostFacts["gpu"];
};

export type WizardEnvironment = {
  id: string;
  name: string;
  servers: WizardServer[];
};

export type WizardProject = {
  id: string;
  name: string;
  environments: WizardEnvironment[];
};

/** A cluster, scoped to the environment it was built in.
 *
 *  It carries no GPU figure. The cluster listing used to publish its largest
 *  node's card so a model could be fit-checked against it, and that number was
 *  deleted along with the whole idea: `llm` is on the control plane's cluster
 *  exclusion list now, so a model endpoint never reaches a cluster and there is
 *  nothing here to size. */
export type WizardCluster = {
  id: string;
  name: string;
  environmentId: string;
  status?: string;
};

/**
 * What the organization actually has to deploy onto. Derived once from the
 * targets the page already loaded — no extra round trip, and no second idea of
 * what "eligible" means.
 */
export type TargetInventory = {
  /** Server types present on at least one environment the user can deploy to. */
  serverTypes: Set<string>;
  /** Clusters, likewise. */
  clusterCount: number;
  /** Kinds a cluster refuses, as the control plane publishes them. */
  clusterExcludedKinds: Set<string>;
  /** The engines THIS control plane has enabled (SIGMA-268), as it publishes
   *  them at /capabilities. Null means nothing has told us — every engine the
   *  catalog defines is assumed, which is the behaviour that predates the
   *  endpoint. Never an empty set: a control plane cannot boot with no engines,
   *  so empty could only ever be a failed read, and narrowing on one would take
   *  away engines a working deployment has. */
  enabledDbEngines: Set<string> | null;
  enabledS3Engines: Set<string> | null;
};

/** The engine sets as the control plane publishes them. */
export type EngineCapabilities = { dbEngines: string[]; s3Engines: string[] };

/**
 * A server the enrollment gate refused is NOT a place to deploy. It matches the
 * availability matrix on paper (its type says so) and not in fact, and the
 * control plane refuses to schedule onto it — so counting it here would put the
 * user back in exactly the dead end this module exists to remove, one screen
 * later (SIGMA-203).
 */
export function serverIsDeployable(server: WizardServer): boolean {
  return server.status !== SERVER_STATUS.incompatible && server.status !== SERVER_STATUS.decommissioning;
}

export function buildInventory(
  projects: WizardProject[],
  clusters: WizardCluster[] = [],
  clusterExcludedKinds: string[] = [],
  capabilities: EngineCapabilities | null = null
): TargetInventory {
  const serverTypes = new Set<string>();
  for (const project of projects) {
    for (const env of project.environments) {
      for (const server of env.servers) {
        if (serverIsDeployable(server)) serverTypes.add(server.type);
      }
    }
  }
  return {
    serverTypes,
    clusterCount: clusters.length,
    clusterExcludedKinds: new Set(clusterExcludedKinds),
    enabledDbEngines: capabilities?.dbEngines.length ? new Set(capabilities.dbEngines) : null,
    enabledS3Engines: capabilities?.s3Engines.length ? new Set(capabilities.s3Engines) : null,
  };
}

/**
 * Whether this control plane has the engine behind a kind enabled (SIGMA-268).
 *
 * A database kind IS its engine — `postgres` the kind and `postgres` the engine
 * are one name — so CP_DB_ENGINES answers directly. Object storage is one kind
 * over several engines, so `s3` is deployable while ANY of them is enabled and
 * the engine picker filters within it.
 *
 * Kinds with no engine behind them (`app`, `llm`) are never gated here: no
 * setting turns them off, and answering "not enabled" for them would be
 * inventing a refusal the control plane would not make.
 */
export function engineEnabled(kind: ResourceKind, inv: TargetInventory): boolean {
  if (kind === "s3") return inv.enabledS3Engines === null || inv.enabledS3Engines.size > 0;
  if (!isDatabaseEngineKind(kind)) return true;
  return inv.enabledDbEngines === null || inv.enabledDbEngines.has(kind);
}

/** A kind whose name is a database engine name, asked of the catalog rather
 *  than listed here — the four names are exactly what SIGMA-216 stopped
 *  retyping. */
function isDatabaseEngineKind(kind: ResourceKind): boolean {
  return kindsInCategory("database").includes(kind);
}

/** Whether a kind may run INSIDE a cluster. The exclusion list is the control
 *  plane's and travels with the cluster listing; an empty list means "nothing
 *  told us otherwise", which is the CP's own default. */
export function clusterEligible(kind: ResourceKind, inv: TargetInventory): boolean {
  return !inv.clusterExcludedKinds.has(kind);
}

export type KindAvailability = {
  available: boolean;
  /** Why not — a sentence, in the operator's terms, not a matrix cell. */
  reason?: string;
  /** The single thing that fixes it. */
  action?: { label: string; href: string };
};

const CONNECT_SERVER = { label: "Connect a server", href: "/dashboard/servers" };

/** "a, b or c" — the form the catalog's own sentences read in. */
function joinOr(items: string[]): string {
  if (items.length === 0) return "";
  if (items.length === 1) return items[0];
  return `${items.slice(0, -1).join(", ")} or ${items[items.length - 1]}`;
}

/**
 * Can this kind be deployed anywhere the user can reach?
 *
 * Availability is deliberately about the FLEET, not about a particular
 * environment: the wizard has not asked for a project yet, and refusing a kind
 * because the first project in the list has no database server would be a
 * different lie than the one this replaces.
 */
export function kindAvailability(kind: ResourceKind, inv: TargetInventory): KindAvailability {
  // Asked FIRST, because it is the one refusal no amount of hardware fixes: a
  // control plane running with CP_DB_ENGINES=postgres will 422 a MongoDB create
  // however many Database servers the org has connected, and telling the
  // operator to connect one would send them to buy a machine for nothing
  // (SIGMA-268).
  if (!engineEnabled(kind, inv)) {
    const label = RESOURCE_KIND_LABELS[kind] ?? kind;
    return {
      available: false,
      reason: `This control plane does not have ${label} enabled. Its engine list is a deployment setting (CP_DB_ENGINES / CP_S3_ENGINES) — an operator has to add it there before it can be created here.`,
    };
  }
  const allowed = (ALLOWED_SERVER_TYPES[kind] ?? []) as ServerType[];
  if (allowed.some((t) => inv.serverTypes.has(t))) return { available: true };
  if (inv.clusterCount > 0 && clusterEligible(kind, inv)) return { available: true };

  // The GPU case gets its own sentence because it is the one where the reason
  // is about HARDWARE, and "connect a General server" would be actively
  // misleading advice.
  //
  // It is reached even by an org whose only deploy target is a Kubernetes
  // cluster, because `llm` is on the control plane's cluster exclusion list —
  // nothing renders a cluster-targeted model endpoint and its provisioning is
  // server-scoped — so the cluster branch above cannot answer "available" for
  // it. An org with one cluster and no GPU server has to be told about the
  // missing hardware; being offered the card and refused at the target step
  // would be the dead end this module exists to remove.
  if (kind === "llm") {
    return {
      available: false,
      reason:
        "No GPU server is connected. A model endpoint runs on NVIDIA hardware with a working driver — connect one and this becomes available.",
      action: CONNECT_SERVER,
    };
  }
  return nothingToRunOn(RESOURCE_KIND_LABELS[kind] ?? kind, allowed, {
    clusterRefused: inv.clusterCount > 0 && !clusterEligible(kind, inv),
  });
}

/**
 * The same verdict for a whole CATEGORY, because that is where step 1 now
 * offers things (SIGMA-216).
 *
 * A card that leads nowhere has to say so where it is offered — the contract a
 * kind card has kept since SIGMA-207 — and after the categories went in front
 * of the kinds, "where it is offered" is the category. It rolls up rather than
 * being decided separately: a category is available when ANY kind inside it is,
 * so a fleet with a Postgres box but no cluster still opens Database, and the
 * kinds inside keep their own sentences.
 *
 * A category holding one kind answers with that kind's own verdict, verbatim.
 * The category IS the kind there — resolving it costs no click (see
 * pickCategory) — so a generated category sentence would be a second, blander
 * way of saying something we already say well: "connect a General server" is
 * not what a missing GPU needs to hear.
 */
export function categoryAvailability(
  category: ResourceCategoryId,
  inv: TargetInventory
): KindAvailability {
  const kinds = kindsInCategory(category);
  const verdicts = kinds.map((kind) => kindAvailability(kind, inv));
  if (verdicts.some((v) => v.available)) return { available: true };
  if (verdicts.length === 1) return verdicts[0];

  // Every kind refused, and more than one of them. The reasons are per-kind
  // ("a PostgreSQL needs one to run on") and stacking four of them under one
  // card is a wall, not an answer — so it is stated once, in the category's
  // terms, over the union of everything that could have hosted any of them.
  const allowed = new Set<ServerType>();
  for (const kind of kinds) {
    for (const type of (ALLOWED_SERVER_TYPES[kind] ?? []) as ServerType[]) allowed.add(type);
  }
  return nothingToRunOn(RESOURCE_CATEGORY_CATALOG[category].label, [...allowed], {
    // Named only when it is true of the WHOLE category: one kind a cluster
    // refuses is not a reason the category is unreachable.
    clusterRefused: inv.clusterCount > 0 && !kinds.some((kind) => clusterEligible(kind, inv)),
  });
}

/** "No X server is connected. A Y needs one to run on." — the one sentence a
 *  kind and a category both end at, so the two cannot drift into saying the
 *  same thing differently. */
function nothingToRunOn(
  label: string,
  allowed: ServerType[],
  opts: { clusterRefused: boolean }
): KindAvailability {
  const types = joinOr(allowed.map((t) => SERVER_TYPE_LABELS[t] ?? t));
  return {
    available: false,
    reason: `No ${types} server is connected${
      opts.clusterRefused
        ? ", and a cluster cannot host this kind — it keeps its data on one host"
        : ""
    }. A ${label} needs one to run on.`,
    action: CONNECT_SERVER,
  };
}

/**
 * Servers in one environment, annotated for the target picker: whether each can
 * host the kind, and if not, why in words rather than as a greyed-out row.
 */
export type ServerOption = {
  server: WizardServer;
  eligible: boolean;
  reason?: string;
  /** Set when the MODEL is what this row was refused for, rather than anything
   *  about the host. A caller summarising a whole environment needs to tell
   *  "every GPU here is too small for this model" from "there is no GPU here",
   *  because the two have different fixes — and telling them apart by matching
   *  on the prose above would be a parser over a sentence we write ourselves. */
  refusedForModel?: boolean;
};

export function serverOptions(
  env: WizardEnvironment | undefined,
  kind: ResourceKind | null | undefined,
  /** The model an `llm` resource will serve, once one has been chosen
   *  (SIGMA-214). Optional, and every other kind passes nothing: a Redis has no
   *  model, and threading the card in as a required argument would make four
   *  call sites pass null to say so. */
  model?: ModelCard | null
): ServerOption[] {
  if (!env || !kind) return [];
  const allowed = new Set((ALLOWED_SERVER_TYPES[kind] ?? []) as string[]);
  const kindLabel = RESOURCE_KIND_LABELS[kind] ?? kind;
  return env.servers.map((server) => {
    const typeLabel = SERVER_TYPE_LABELS[server.type as ServerType] ?? server.type;
    if (server.status === SERVER_STATUS.incompatible) {
      // Its type says yes and the machine says no. Saying "incompatible" alone
      // reads as a bug in the matrix, so name which of the two is wrong.
      return {
        server,
        eligible: false,
        reason: `This host was refused as a ${typeLabel} server — change its type or fix the host before scheduling work onto it.`,
      };
    }
    if (server.status === SERVER_STATUS.decommissioning) {
      return { server, eligible: false, reason: "This server is being decommissioned." };
    }
    if (!allowed.has(server.type)) {
      return {
        server,
        eligible: false,
        reason: `A ${typeLabel} server cannot host a ${kindLabel}.`,
      };
    }
    // Last, because it is the only reason here that depends on a choice made on
    // an EARLIER step rather than on the host itself: the model. A GPU server
    // whose card is too small for the chosen model is a target the control
    // plane's create call refuses (store.checkModelFits) — so it is refused
    // here too, in the same terms, one screen earlier and for free.
    const fit = serverFitsModel(model, server);
    if (!fit.fits) {
      return { server, eligible: false, reason: fit.reason, refusedForModel: true };
    }
    return { server, eligible: true };
  });
}

/** A cluster, annotated exactly as a server is. */
export type ClusterOption = {
  cluster: WizardCluster;
  eligible: boolean;
  reason?: string;
};

/**
 * Clusters offered for an environment. A cluster picker that showed every
 * cluster in the org would offer a target the control plane refuses — a cluster
 * belongs to exactly one environment, and it says so.
 *
 * No model is compared here, and that is not the gap it was. A cluster row used
 * to be fit-checked against the largest GPU its nodes reported, because the
 * create call refused an oversized model and the wizard could otherwise only
 * find that out from the 422 (SIGMA-214). The control plane now refuses the
 * KIND instead: `llm` is on its cluster exclusion list, so a model endpoint is
 * turned away by the eligibility branch below — before any card size could
 * matter — and the cluster listing stopped publishing the figure. A comparison
 * kept here would be reading a field nothing sends.
 */
/**
 * Why a kind the control plane excluded runs outside the cluster.
 *
 * There are two different reasons and they were being told as one. The managed
 * engines are excluded because a stateful container rescheduled onto a node
 * without its data is data loss — that is the sentence, and it is true of
 * Postgres, MySQL, MongoDB, Redis and object storage. It is NOT true of a model
 * endpoint, which keeps nothing it cannot re-download; that one is excluded
 * because the cluster renderer has no path for it, and telling an operator
 * their inference server is a database they might lose data from is a sentence
 * that answers a question they did not ask and leaves theirs open.
 */
export function outsideClusterReason(kind: ResourceKind): string {
  const label = RESOURCE_KIND_LABELS[kind] ?? kind;
  if (kind === "llm") {
    return `${label} runs on a GPU server of its own rather than inside a cluster — the scheduler has no path for a model endpoint yet.`;
  }
  return `${label} keeps its data on one host, so it runs on its own server rather than inside a cluster.`;
}

export function clusterOptions(
  clusters: WizardCluster[],
  environmentId: string,
  kind: ResourceKind | null | undefined,
  inv: TargetInventory
): ClusterOption[] {
  if (!environmentId || !kind) return [];
  return clusters
    .filter((c) => c.environmentId === environmentId)
    .map((cluster) => {
      if (!clusterEligible(kind, inv)) {
        return { cluster, eligible: false, reason: outsideClusterReason(kind) };
      }
      if (cluster.status === "provisioning") {
        return { cluster, eligible: false, reason: "This cluster is still coming up." };
      }
      return { cluster, eligible: true };
    });
}

/** Every offer the target step renders, and the sentence to show when it can
 *  render nothing pickable. */
export type TargetChoices = {
  /** The chosen project's environments — the environment select's options. */
  environments: WizardEnvironment[];
  servers: ServerOption[];
  clusters: ClusterOption[];
  /** Set only when there WERE offers and every one of them was refused. An
   *  environment with nothing attached is a different state with its own
   *  sentence, and saying "nothing here can run this model" about an empty
   *  environment would name the wrong cause. */
  deadEnd: string | null;
};

/**
 * The target step's whole content, decided in one pure place.
 *
 * It exists because the step's rendering is not what was ever wrong: the
 * arguments were. `model` reached serverOptions and never reached
 * clusterOptions, so a cluster was offered for a model no node could hold, and
 * nothing could catch it because the wiring lived in JSX — this repository's
 * suites run in node with no DOM, so a component's props are exactly the thing
 * they cannot see. Assembling them here makes the wiring a value a test can
 * hold, which is the same reason createResourceInput and reviewSummary are
 * functions rather than inline objects.
 */
export function targetChoices(input: {
  projects: WizardProject[];
  clusters: WizardCluster[];
  inventory: TargetInventory;
  kind: ResourceKind | null | undefined;
  /** The chosen model, for an `llm`. Every other kind passes nothing and is
   *  filtered exactly as before. */
  model?: ModelCard | null;
  projectId: string;
  environmentId: string;
}): TargetChoices {
  const environments = input.projects.find((p) => p.id === input.projectId)?.environments ?? [];
  const env = environments.find((e) => e.id === input.environmentId);
  const servers = serverOptions(env, input.kind, input.model);
  // The model reaches the SERVER filter only: a cluster is refused for the kind
  // (`llm` is excluded control-plane side), so there is no cluster row a model
  // could be measured against.
  const clusters = clusterOptions(
    input.clusters,
    input.environmentId,
    input.kind,
    input.inventory
  );
  return { environments, servers, clusters, deadEnd: deadEnd(servers, clusters, input.model) };
}

/**
 * The one sentence for an environment where every row is refused.
 *
 * Each row already carries its own reason, and a column of them still leaves
 * "so what do I do" unanswered — the fix for "no GPU here is big enough" is a
 * different model, and it is not deducible from a list of servers. Which
 * sentence to show is decided from the refusals themselves rather than from the
 * kind, because an environment can hold a too-small GPU and a Postgres box at
 * once and only one of those facts is worth acting on.
 */
function deadEnd(
  servers: ServerOption[],
  clusters: ClusterOption[],
  model?: ModelCard | null
): string | null {
  const offers = [...servers, ...clusters];
  if (offers.length === 0 || offers.some((o) => o.eligible)) return null;
  // Servers only: a cluster is never refused FOR the model — it is refused for
  // the kind, and "pick a smaller model" is no help to someone whose target
  // cannot host a model endpoint at any size.
  if (model && servers.some((s) => s.refusedForModel)) {
    return `Nothing in this environment can run this model — it needs about ${vramNeedText(
      model
    )} of VRAM and every GPU here is smaller. Pick a smaller or quantized build of it (an AWQ or GPTQ repository of the same model), or an environment with a bigger card.`;
  }
  return "Nothing in this environment can host this resource. Attach a compatible server to it, or pick a different environment.";
}
