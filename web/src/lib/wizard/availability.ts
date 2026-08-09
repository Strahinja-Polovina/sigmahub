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
 * renders it.
 */

import {
  ALLOWED_SERVER_TYPES,
  RESOURCE_KIND_LABELS,
  SERVER_TYPE_LABELS,
  type ResourceKind,
  type ServerType,
} from "@/lib/server-catalog.generated";
import { SERVER_STATUS } from "@/lib/server-compat";

/** A server as the wizard sees it — the deploy-target shape, plus the status
 *  the enrollment gate wrote. */
export type WizardServer = {
  id: string;
  name: string;
  type: string;
  provider?: string;
  region?: string;
  status?: string;
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

/** A cluster, scoped to the environment it was built in. */
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
};

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
  clusterExcludedKinds: string[] = []
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
  };
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
  const allowed = (ALLOWED_SERVER_TYPES[kind] ?? []) as ServerType[];
  if (allowed.some((t) => inv.serverTypes.has(t))) return { available: true };
  if (inv.clusterCount > 0 && clusterEligible(kind, inv)) return { available: true };

  const types = joinOr(allowed.map((t) => SERVER_TYPE_LABELS[t] ?? t));
  const label = RESOURCE_KIND_LABELS[kind] ?? kind;

  // The GPU case gets its own sentence because it is the one where the reason
  // is about HARDWARE, and "connect a General server" would be actively
  // misleading advice.
  if (kind === "llm") {
    return {
      available: false,
      reason:
        "No GPU server is connected. A model endpoint runs on NVIDIA hardware with a working driver — connect one and this becomes available.",
      action: CONNECT_SERVER,
    };
  }
  return {
    available: false,
    reason: `No ${types} server is connected${
      inv.clusterCount > 0 && !clusterEligible(kind, inv)
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
};

export function serverOptions(
  env: WizardEnvironment | undefined,
  kind: ResourceKind | null | undefined
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
    return { server, eligible: true };
  });
}

/**
 * Clusters offered for an environment. A cluster picker that showed every
 * cluster in the org would offer a target the control plane refuses — a cluster
 * belongs to exactly one environment, and it says so.
 */
export function clusterOptions(
  clusters: WizardCluster[],
  environmentId: string,
  kind: ResourceKind | null | undefined,
  inv: TargetInventory
): { cluster: WizardCluster; eligible: boolean; reason?: string }[] {
  if (!environmentId || !kind) return [];
  return clusters
    .filter((c) => c.environmentId === environmentId)
    .map((cluster) => {
      if (!clusterEligible(kind, inv)) {
        return {
          cluster,
          eligible: false,
          reason: `${
            RESOURCE_KIND_LABELS[kind] ?? kind
          } keeps its data on one host, so it runs on its own server rather than inside a cluster.`,
        };
      }
      if (cluster.status === "provisioning") {
        return { cluster, eligible: false, reason: "This cluster is still coming up." };
      }
      return { cluster, eligible: true };
    });
}
