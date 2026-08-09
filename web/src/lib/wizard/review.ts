/**
 * The one review screen (SIGMA-211).
 *
 * The old wizard's last screen before Deploy was the variables table, so the
 * last thing a user saw was never the thing they were about to create. Every
 * decision made across six screens is restated here, in the order it was made,
 * so that "wait, that's staging" is caught before the create call rather than
 * after the resource exists.
 *
 * It is a pure function over the draft because a summary that is assembled
 * inline in JSX is a summary nothing can check — and a review screen that omits
 * a field is worse than no review screen, since it implies the omitted field
 * was not set.
 */

import { RESOURCE_KIND_LABELS, type ResourceKind } from "@/lib/server-catalog.generated";
import { BUILD_METHOD_LABELS, type BuildMethod } from "./build";
import { llmEngineLabel } from "./managed";
import { reachability, type PortMapping } from "./networking";

export type ReviewInput = {
  kind: ResourceKind;
  name: string;
  /** App path. */
  repo?: string;
  branch?: string;
  buildMethod?: BuildMethod | null;
  dockerfile?: string;
  contextSubdir?: string;
  composeServiceCount?: number;
  ports?: PortMapping[];
  domain?: string;
  healthPath?: string;
  envVarCount?: number;
  /** Managed path. */
  engineVersion?: string;
  llmEngine?: string;
  llmModel?: string;
  /** Target, both paths. */
  projectName?: string;
  environmentName?: string;
  serverName?: string;
  clusterName?: string;
};

export type ReviewRow = {
  label: string;
  value: string;
  /** A second line, when the value alone would be true but not informative. */
  hint?: string;
  /** The step to jump back to when the user disagrees. */
  step?: string;
};

/**
 * Every decision, in one list.
 *
 * A row whose value is unknown is OMITTED rather than shown as "—": a review
 * screen listing "Domain: —" invites the reading that a domain was configured
 * and is empty, when the truth is that the flow never asked.
 */
export function reviewSummary(input: ReviewInput): ReviewRow[] {
  const rows: ReviewRow[] = [];

  rows.push({
    label: "Type",
    value: RESOURCE_KIND_LABELS[input.kind] ?? input.kind,
    step: "kind",
  });
  if (input.name.trim()) {
    rows.push({ label: "Name", value: input.name.trim(), step: "kind" });
  }

  if (input.repo) {
    rows.push({
      label: "Source",
      value: input.branch ? `${input.repo} @ ${input.branch}` : input.repo,
      step: "source",
    });
  }

  if (input.buildMethod) {
    const parts: string[] = [];
    if (input.contextSubdir) parts.push(`context ${input.contextSubdir}`);
    if (input.dockerfile && input.buildMethod === "dockerfile") parts.push(input.dockerfile);
    if (input.buildMethod === "compose" && input.composeServiceCount) {
      parts.push(
        `${input.composeServiceCount} ${input.composeServiceCount === 1 ? "service" : "services"}`
      );
    }
    rows.push({
      label: "Build",
      value: BUILD_METHOD_LABELS[input.buildMethod] ?? input.buildMethod,
      hint: parts.length ? parts.join(" · ") : undefined,
      step: "build",
    });
  }

  if (input.ports) {
    const listed = input.ports
      .filter((p) => p.container > 0)
      .map((p) => (p.host === 0 ? `${p.container}` : `${p.host}→${p.container}`));
    const { summary } = reachability(input.ports, input.domain ?? "");
    rows.push({
      label: "Ports",
      value: listed.length ? listed.join(", ") : "none",
      hint: summary,
      step: "networking",
    });
    if (input.healthPath) {
      rows.push({ label: "Health check", value: input.healthPath, step: "networking" });
    }
  }

  if (input.domain?.trim()) {
    rows.push({ label: "Domain", value: input.domain.trim(), step: "networking" });
  }

  if (input.engineVersion) {
    rows.push({ label: "Version", value: input.engineVersion, step: "engine" });
  }
  if (input.llmEngine) {
    rows.push({
      label: "Runtime",
      // The same label the model step showed. Printing the id here read as a
      // different answer to the same question one screen later.
      value: llmEngineLabel(input.llmEngine),
      hint: input.llmModel || undefined,
      step: "model",
    });
  }

  const target = input.clusterName
    ? `cluster ${input.clusterName}`
    : input.serverName ?? "";
  if (input.projectName || target) {
    const where = [input.projectName, input.environmentName].filter(Boolean).join(" / ");
    rows.push({
      label: "Target",
      value: where || target,
      hint: where && target ? target : undefined,
      step: "target",
    });
  }

  if (typeof input.envVarCount === "number") {
    rows.push({
      label: "Variables",
      value:
        input.envVarCount === 0
          ? "none"
          : `${input.envVarCount} ${input.envVarCount === 1 ? "variable" : "variables"}`,
      step: "env",
    });
  }

  return rows;
}

/**
 * What is still missing before the create call can be made.
 *
 * The Deploy button is disabled with these as its reason. Listing them beats a
 * greyed-out button whose cause is on a screen the user has already left —
 * which is the pattern the rest of the dashboard uses and this flow did not.
 */
export function blockingGaps(input: ReviewInput): string[] {
  const gaps: string[] = [];
  if (!input.name.trim()) gaps.push("The resource needs a name.");
  if (!input.projectName) gaps.push("Pick a project.");
  if (!input.environmentName) gaps.push("Pick an environment.");
  if (!input.serverName && !input.clusterName) gaps.push("Pick a server or a cluster.");
  if (input.kind === "app") {
    if (!input.repo) gaps.push("Pick a repository.");
    if (!input.buildMethod) gaps.push("Choose how this repository gets built.");
  }
  if (input.kind === "llm" && !input.llmModel?.trim()) {
    gaps.push("Name the model this endpoint serves.");
  }
  return gaps;
}
