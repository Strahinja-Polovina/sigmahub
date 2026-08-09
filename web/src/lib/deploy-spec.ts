/**
 * What the New Resource flow actually sends to the control plane.
 *
 * Two decisions used to be made inline in the createResource server action and
 * both were made wrongly:
 *
 *   - the repo's Compose service graph was dropped on the floor, so a repo
 *     describing five services was created as a one-container app, `spec.compose`
 *     was never written, and the reconciler's entire per-service branch was
 *     unreachable from the product (SIGMA-199);
 *   - the target was assumed to be a server, so a cluster deploy was refused
 *     with "A target server is required" (SIGMA-200).
 *
 * They live here because they are pure decisions about a request body: no auth,
 * no database, no `fetch`. That makes them testable without a control plane,
 * which is the only reason either bug would have been caught.
 */

/** One service exactly as the CP's inspector emits it. Structurally identical
 *  to `CpDetectedComposeService` in @/server/cp, restated here so this module
 *  stays free of server-only imports. */
export type DetectedComposeService = {
  name: string;
  build?: string;
  dockerfile?: string;
  image?: string;
  ports?: number[];
  publishedPorts?: number[];
  namedVolumes?: string[];
  dependsOn?: string[];
  rollout?: string;
};

/** One service as it is PERSISTED in `spec.compose.services`. Same field names
 *  as the detected shape — the reconciler (cp/internal/reconciler/container.go
 *  composeServiceSpec) and the placement store (store.ComposeServiceView) read
 *  this document directly, so renaming anything here silently unbuilds a
 *  service rather than failing. */
export type ComposeSpecService = DetectedComposeService & { rollout: string };

/** Swap classes, mirroring gitdetect's constants. "recreate" means the service
 *  is stopped before its replacement starts — the documented per-service
 *  exception to the zero-downtime guarantee. */
export const ROLLOUT_BLUE_GREEN = "blue-green";
export const ROLLOUT_RECREATE = "recreate";

/**
 * Build the `spec.compose` block from a detection result, or null when the repo
 * is a plain single-container app.
 *
 * EVERY detected service is carried through, including one the reconciler will
 * later filter out for having neither a build context nor an image: the spec is
 * the record of what the repository declares, and a service that quietly
 * vanished between detection and storage is precisely the failure this replaces.
 * Filtering is the renderer's decision, made at render time, from the full set.
 */
export function composeSpecFromDetected(
  services: DetectedComposeService[] | undefined
): { services: ComposeSpecService[] } | null {
  if (!services || services.length === 0) return null;
  const out: ComposeSpecService[] = [];
  for (const svc of services) {
    const name = svc.name?.trim();
    if (!name) continue; // an unnamed service can't be addressed by anything
    out.push({
      name,
      ...(svc.build ? { build: svc.build } : {}),
      ...(svc.dockerfile ? { dockerfile: svc.dockerfile } : {}),
      ...(svc.image ? { image: svc.image } : {}),
      ...(svc.ports?.length ? { ports: [...svc.ports] } : {}),
      ...(svc.publishedPorts?.length ? { publishedPorts: [...svc.publishedPorts] } : {}),
      ...(svc.namedVolumes?.length ? { namedVolumes: [...svc.namedVolumes] } : {}),
      ...(svc.dependsOn?.length ? { dependsOn: [...svc.dependsOn] } : {}),
      // Default to blue-green rather than leaving it empty: an absent rollout
      // reads as blue-green downstream anyway, and writing the verdict down
      // means the stored spec answers "how does this service swap?" on its own.
      rollout: svc.rollout === ROLLOUT_RECREATE ? ROLLOUT_RECREATE : ROLLOUT_BLUE_GREEN,
    });
  }
  return out.length > 0 ? { services: out } : null;
}

/**
 * Why a service cannot swap without going down, or null when it can.
 *
 * The rule is the CP's (gitdetect/compose.go): a named volume is exclusive
 * state, and a fixed host port cannot be held by two generations at once. The
 * CP already decided — `rollout` is the verdict — so this only explains it. When
 * a service is marked recreate with neither piece of evidence present we still
 * say something true rather than nothing, because a badge with no explanation is
 * how "recreate" comes to look like an arbitrary product decision.
 */
export function recreateReason(svc: DetectedComposeService): string | null {
  if (svc.rollout !== ROLLOUT_RECREATE) return null;
  const causes: string[] = [];
  if (svc.namedVolumes?.length) {
    causes.push(
      `it mounts the named volume${svc.namedVolumes.length > 1 ? "s" : ""} ${svc.namedVolumes.join(", ")}`
    );
  }
  if (causes.length === 0) return "it holds a resource only one copy can own at a time";
  return causes.join(" and ");
}

/** Every service that will go down during a deploy, with the reason. Empty
 *  means the whole app swaps blue-green — i.e. the zero-downtime promise the
 *  rest of the UI makes holds for this repo. */
export function recreateSummary(
  services: DetectedComposeService[] | undefined
): { name: string; reason: string }[] {
  if (!services) return [];
  const out: { name: string; reason: string }[] = [];
  for (const svc of services) {
    const reason = recreateReason(svc);
    if (reason) out.push({ name: svc.name, reason });
  }
  return out;
}

/** A validated deploy target: exactly one of a server or a cluster. */
export type DeployTargetInput = {
  serverId?: string | null;
  clusterId?: string | null;
};
export type ResolvedDeployTarget =
  | { ok: true; serverId?: string; clusterId?: string }
  | { ok: false; error: string };

/**
 * Resolve which of the two mutually exclusive targets the caller asked for.
 *
 * The messages match the control plane's, because this check exists only to say
 * the same thing one round trip earlier — a divergent wording here would be a
 * second, quieter rule the operator has to reverse-engineer.
 */
export function resolveDeployTarget(input: DeployTargetInput): ResolvedDeployTarget {
  const serverId = input.serverId?.trim() ?? "";
  const clusterId = input.clusterId?.trim() ?? "";
  if (serverId && clusterId) {
    return {
      ok: false,
      error:
        "Pick either a server or a cluster for this resource — it runs on one server or inside a cluster, not both.",
    };
  }
  if (!serverId && !clusterId) {
    return {
      ok: false,
      error:
        "A deploy target is required: pick a server to run this on, or a cluster to run it in.",
    };
  }
  return serverId ? { ok: true, serverId } : { ok: true, clusterId };
}

// Which KINDS a cluster refuses is deliberately not restated here. It is a
// domain rule the control plane owns and already publishes (GET /clusters
// returns `excludedKinds` for exactly this reason), and a second copy in the
// dashboard is how the two drift into disagreeing about what is deployable.

/**
 * The host-port bindings a compose file asked for and SigmaHub will not make.
 *
 * `ports: "3000:3000"` reads as a promise that the host will answer on 3000. It
 * will not: compose ports are EXPOSED so Traefik can reach the container, never
 * published, because a host binding collides the moment two apps ask for the
 * same port and ingress is the proxy's job. Saying so beats silently dropping a
 * line the user wrote — and beats the previous answer, which was to force the
 * service into a downtime-taking rollout and blame a binding that never existed.
 */
export function ignoredHostPorts(
  services: readonly DetectedComposeService[] | undefined
): { name: string; ports: number[] }[] {
  const out: { name: string; ports: number[] }[] = [];
  for (const svc of services ?? []) {
    if (svc.publishedPorts?.length) {
      out.push({ name: svc.name, ports: [...svc.publishedPorts] });
    }
  }
  return out;
}

/** The subset of the create-resource input that shapes the persisted spec. */
export type ResourceSpecInput = {
  repo?: string | null;
  domain?: string | null;
  detected?: {
    ports?: number[];
    healthCheck?: { type?: string; path?: string; port?: number; intervalSec?: number } | null;
    services?: DetectedComposeService[];
  } | null;
  /** The wizard's port mappings, when it collected them. Overrides the detected
   *  ports, because the whole point of showing them was to let the user change
   *  them (SIGMA-210). */
  ports?: { container: number; host: number; protocol?: string }[] | null;
  /** How this repository gets built (SIGMA-209). The agent's image.build op has
   *  taken a dockerfile path and a context subdirectory since the Compose work;
   *  nothing on the single-container path ever set either, so a repo whose
   *  Dockerfile is not at its root could not be deployed at all. */
  build?: { method: string; dockerfile?: string; contextSubdir?: string } | null;
  llm?: { engine?: string; model?: string } | null;
  /** Object-storage engine (minio | seaweedfs). Rides the SAME `engine` spec
   *  field as the LLM runtime, because that is what the control plane reads for
   *  both — see store.s3EngineFromSpec and store.provisionLLMTx. */
  s3Engine?: string | null;
};

/**
 * Build the resource spec the control plane persists.
 *
 * Extracted from `createResource` so it can be tested at all. It was inline,
 * and that is exactly why SIGMA-199 shipped: the one statement that turns a
 * detected compose graph into `spec.compose` could be deleted and every suite
 * stayed green, because nothing anywhere asserted the wiring — only the pure
 * helpers on either side of it.
 */
export function buildResourceSpec(input: ResourceSpecInput): Record<string, unknown> {
  const spec: Record<string, unknown> = {
    repo: input.repo ?? null,
    domain: input.domain ?? null,
  };
  // `ports` drives the rollout's exposed ports AND the default TCP health
  // probe; host 0 means internal-only, which is the safe default — Traefik
  // fronts anything that needs to be reachable.
  //
  // The wizard's mappings win over detection when it collected them: detection
  // is a pre-fill the user is shown precisely so they can correct it, and
  // re-deriving from the raw detected list here would silently discard the
  // correction (and the published host port that came with it).
  const explicit = (input.ports ?? []).filter(
    (p) => Number.isInteger(p.container) && p.container > 0 && p.container < 65536
  );
  if (explicit.length > 0) {
    spec.ports = explicit.map((p) => ({
      container: p.container,
      host: Number.isInteger(p.host) && p.host > 0 && p.host < 65536 ? p.host : 0,
      protocol: p.protocol ?? "tcp",
    }));
  } else {
    const ports = (input.detected?.ports ?? []).filter(
      (p) => Number.isInteger(p) && p > 0 && p < 65536
    );
    if (ports.length > 0) {
      spec.ports = ports.map((container) => ({ container, host: 0, protocol: "tcp" }));
    }
  }
  if (input.detected?.healthCheck?.type) {
    spec.healthCheck = input.detected.healthCheck;
  }
  // Where and how to build. Absent means the historical default — a Dockerfile
  // at the clone root — which is what every pre-wizard resource carries and
  // what the reconciler still assumes when the block is missing.
  if (input.build?.method) {
    spec.build = {
      method: input.build.method,
      ...(input.build.dockerfile ? { dockerfile: input.build.dockerfile } : {}),
      ...(input.build.contextSubdir ? { contextSubdir: input.build.contextSubdir } : {}),
    };
  }
  // A Compose repo is a graph, and `spec.compose` is the only thing that makes
  // the control plane treat it as one: without it the reconciler takes the
  // single-container path and the other services are never built, never started
  // and never mentioned anywhere (SIGMA-199).
  const compose = composeSpecFromDetected(input.detected?.services);
  if (compose) {
    spec.compose = compose;
  }
  // An inference endpoint is defined by what it runs: without these the control
  // plane has nothing to render and refuses the create.
  if (input.llm?.engine) {
    spec.engine = input.llm.engine;
    spec.model = input.llm.model;
  }
  // Object storage picks an engine too, through the same field. The two never
  // co-occur — a resource is one kind — so one key is not an ambiguity.
  if (input.s3Engine) {
    spec.engine = input.s3Engine;
  }
  return spec;
}

/**
 * The body `POST /resources` is sent.
 *
 * A pure function for the same reason buildResourceSpec is one: dropping
 * `clusterId` from the payload was SIGMA-200, and it could be dropped again
 * with every check still green, because nothing on the web side asserted what
 * goes on the wire.
 *
 * Both target ids are always present, as "" when unset, so the control plane's
 * exactly-one-target check sees the same absence whichever field the caller
 * left out — an omitted key and an empty one must not mean different things.
 */
export function createResourceBody(input: {
  environmentId: string;
  serverId?: string;
  clusterId?: string;
  name: string;
  kind: string;
  spec?: Record<string, unknown>;
}): Record<string, unknown> {
  return {
    environmentId: input.environmentId,
    serverId: input.serverId ?? "",
    clusterId: input.clusterId ?? "",
    name: input.name,
    kind: input.kind,
    spec: input.spec ?? {},
  };
}
