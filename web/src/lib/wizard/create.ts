/**
 * The create call the wizard makes, assembled as a pure function.
 *
 * This exists because of what keeps going wrong here. The last four
 * regressions in this flow were all the same shape: a field the whole stack
 * understood, dropped by the ONE statement that had to put it in the request —
 * the Compose service graph (SIGMA-199), the cluster id (SIGMA-200), the
 * detected ports (SIGMA-160). Every one of them could be deleted with every
 * suite green, because the pure helpers on both sides of the assignment were
 * covered and the assignment was not.
 *
 * Assembling the payload inline in a dialog component is what made that
 * possible: there is no way to assert it without rendering React. So the
 * component now decides nothing — it holds state and calls this.
 */

import type { ResourceKind } from "@/lib/server-catalog.generated";
import { BUILD_COMPOSE, buildSpecFor, type BuildMethod, type DetectedRepo } from "./build";
import {
  defaultHealthPort,
  healthCheckSpec,
  specPorts,
  type PortMapping,
} from "./networking";
import { submittableEnvVars, type EnvDraft } from "./env";
import { isManagedKind } from "./managed";

/** Everything the wizard holds that the create call reads. */
export type WizardDraftState = {
  kind: ResourceKind;
  name: string;
  projectId: string;
  environmentId: string;
  serverId: string;
  clusterId: string;
  domain: string;
  repo: { fullName: string; installationId?: string } | null;
  branch: string;
  detected: DetectedRepo | null;
  method: BuildMethod | null;
  dockerfile: string;
  contextSubdir: string;
  ports: PortMapping[];
  healthPath: string;
  s3Engine: string;
  llmEngine: string;
  llmModel: string;
  envVars: EnvDraft[];
};

/** The argument object `createResource` takes. Structurally identical to the
 *  server action's input, restated here so this module imports no server code. */
export type CreateResourceArgs = {
  projectId: string;
  environmentId: string;
  serverId?: string;
  clusterId?: string;
  name: string;
  kind: string;
  domain?: string;
  repo?: string;
  installationId?: string;
  detected?: {
    ports?: number[];
    healthCheck?: { type: string; path?: string; port?: number; intervalSec?: number };
    services?: DetectedRepo["services"];
  };
  ports?: { container: number; host: number; protocol?: string }[];
  build?: { method: string; dockerfile?: string; contextSubdir?: string };
  llm?: { engine: string; model: string };
  s3Engine?: string;
};

/**
 * Build the create request from the wizard's state.
 *
 * The three type-shaped rules, each of which was a bug at some point:
 *
 *   - A COMPOSE app sends its service graph and nothing about ports or a build
 *     block. Its ports belong to its services and its build instructions are
 *     per service; a top-level build block would name one context for an app
 *     that has several, and a top-level port list would publish the wrong one.
 *   - A NON-COMPOSE app sends the mappings the user left the networking step
 *     with, plus the build decision — never the raw detected list, which is a
 *     pre-fill they were shown precisely so they could change it.
 *   - A MANAGED kind sends none of that. It has no repository, so a `repo`
 *     field on it is a git connection the control plane will try to derive for
 *     a resource that has no commits.
 */
export function createResourceInput(d: WizardDraftState): CreateResourceArgs {
  const isApp = d.kind === "app";
  const compose = isApp && d.method === BUILD_COMPOSE;

  const args: CreateResourceArgs = {
    projectId: d.projectId,
    environmentId: d.environmentId,
    name: d.name.trim(),
    kind: d.kind,
  };
  // Exactly one target. Sending "" for the other would be sending a value the
  // control plane's exactly-one check has to special-case.
  if (d.serverId) args.serverId = d.serverId;
  if (d.clusterId) args.clusterId = d.clusterId;
  if (d.domain.trim()) args.domain = d.domain.trim();

  if (isApp && d.repo) {
    args.repo = d.repo.fullName;
    if (d.repo.installationId) args.installationId = d.repo.installationId;
  }

  if (isApp && d.detected) {
    args.detected = {
      ports: d.detected.ports,
      // A compose app keeps the probe the compose file declared; a single
      // container gets the one the user just confirmed on the network step.
      healthCheck: compose
        ? d.detected.healthCheck?.type
          ? {
              type: d.detected.healthCheck.type,
              path: d.detected.healthCheck.path,
              port: d.detected.healthCheck.port,
              intervalSec: d.detected.healthCheck.intervalSec,
            }
          : undefined
        : healthCheckSpec({
            path: d.healthPath,
            port: defaultHealthPort(d.detected.healthCheck, d.ports),
            detected: d.detected.healthCheck,
          }),
      // The graph is what makes the control plane treat this as a graph:
      // without it the reconciler takes the single-container path and the
      // other services are never built, started or mentioned (SIGMA-199).
      services: compose ? d.detected.services : undefined,
    };
  }

  if (isApp && !compose) {
    const ports = specPorts(d.ports);
    if (ports.length > 0) args.ports = ports;
    const build = buildSpecFor({
      method: d.method,
      dockerfile: d.dockerfile,
      contextSubdir: d.contextSubdir,
    });
    if (build) args.build = build;
  }

  if (d.kind === "llm") {
    args.llm = { engine: d.llmEngine, model: d.llmModel.trim() };
  }
  if (d.kind === "s3") {
    args.s3Engine = d.s3Engine;
  }
  return args;
}

/** Variables that become secrets after the resource exists. A managed engine
 *  generates its own credentials, so the flow never collected any. */
export function createSecretsFor(d: WizardDraftState): { key: string; value: string }[] {
  if (isManagedKind(d.kind)) return [];
  return submittableEnvVars(d.envVars).map((v) => ({ key: v.key.trim(), value: v.value }));
}

/** Whether the create should also wire push-to-deploy for this resource. */
export function shouldWireRepo(d: WizardDraftState, cpMode: boolean): boolean {
  return cpMode && d.kind === "app" && Boolean(d.repo?.fullName);
}
