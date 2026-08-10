/**
 * The managed-data path: databases and object storage (SIGMA-212).
 *
 * A managed engine has no repository, no build, no ports to map and no
 * variables to set — the image is the control plane's, the port is the
 * engine's, and the credentials are generated. Everything the old wizard asked
 * for on the way here was about a git repo it does not have, which is why a
 * Redis took five screens to make two decisions.
 */

import {
  DEFAULT_LLM_ENGINE,
  DEFAULT_S3_ENGINE,
  LLM_ENGINE_NAMES,
  RESOURCE_KIND_LABELS,
  S3_ENGINE_NAMES,
  categoryForKind,
  type LLMEngine,
  type ResourceKind,
  type S3Engine,
} from "@/lib/server-catalog.generated";

/**
 * Asked of the control plane's catalog rather than of a list kept here.
 *
 * The list used to be the four engine names, typed out beside a control plane
 * that already knew the same fact — so adding an engine meant remembering a
 * table in the dashboard, and forgetting it meant a new database whose
 * credentials were never revealed after create. The category IS that fact now
 * (SIGMA-216). The names are deliberately not repeated here either: a comment
 * is a copy of the vocabulary that nothing generates and nothing checks, which
 * is the smallest and longest-lived form of this same bug.
 */
export function isDatabaseKind(kind: ResourceKind | null | undefined): boolean {
  return !!kind && categoryForKind(kind) === "database";
}

/** Kinds that need no repository, no build and no variables. */
export function isManagedKind(kind: ResourceKind | null | undefined): boolean {
  return isDatabaseKind(kind) || kind === "s3";
}

/**
 * How each object-storage engine is described in the picker.
 *
 * Only the COPY lives here. Which engines exist, and which one is the default,
 * come from the generated catalog — the control plane refuses an engine it does
 * not know, so a list assembled here could only ever agree with it by
 * coincidence, and it did not: a third engine added to the Go catalog was
 * provisioned by the control plane, described by the demo, and never offered by
 * this picker, while renaming one left the wizard sending a value the create
 * call rejects with a 422 after the dialog has closed — the exact failure the
 * comment on this list used to claim enumeration prevents.
 *
 * Typed as a total Record so that adding an engine to the Go catalog does not
 * silently drop it from the picker: it stops compiling until someone writes the
 * sentence a customer reads before choosing it.
 *
 * The VERSIONS are deliberately absent: the CP pins one image per engine, so
 * offering a version picker would be offering a choice it ignores.
 */
const S3_ENGINE_COPY: Record<S3Engine, { label: string; detail: string }> = {
  minio: {
    label: "MinIO",
    detail: "The default. Widely deployed, strict S3 API compatibility.",
  },
  seaweedfs: {
    label: "SeaweedFS",
    detail: "Lighter on memory for very large object counts.",
  },
};

/** The picker's options, in the catalog's own order. */
export const S3_ENGINES = S3_ENGINE_NAMES.map((id) => ({ id, ...S3_ENGINE_COPY[id] }));

// Re-exported rather than derived from S3_ENGINES[0]: the default is the
// control plane's choice, and "whichever the catalog happens to list first" is
// a different statement that was true only by luck.
export { DEFAULT_S3_ENGINE };

/**
 * How each inference runtime is described in the picker.
 *
 * Same story as S3_ENGINE_COPY above, and the last hand-kept copy in this file
 * (SIGMA-278): the runtimes were a two-entry literal beside a control plane
 * that already knew the same fact. Renaming or replacing the default runtime in
 * store.llmEngines — vllm → vllm-openai, or making ollama the default — left
 * the wizard sending engine "vllm" for every model whose card did not resolve
 * (a Hub timeout, a control plane with no Hub catalogue, a pasted repo id the
 * Hub does not know), and provisionLLMTx answers that with `unknown inference
 * runtime "vllm"` — a 422 at the end of the LLM wizard, with every Go and
 * TypeScript suite green.
 *
 * Only the COPY lives here, typed as a total Record so a runtime added on the
 * Go side stops the build until someone writes the sentence an operator picks
 * it by, rather than appearing as a bare id or not at all.
 */
const LLM_ENGINE_COPY: Record<LLMEngine, { label: string; detail: string }> = {
  vllm: { label: "vLLM", detail: "High-throughput serving for most open models" },
  ollama: { label: "Ollama", detail: "Simple single-model serving" },
};

/** The picker's options, in the control plane's own order. */
export const LLM_ENGINES = LLM_ENGINE_NAMES.map((id) => ({ id, ...LLM_ENGINE_COPY[id] }));

// Re-exported rather than derived from LLM_ENGINES[0]: the default is the
// control plane's choice (store.DefaultLLMEngine), and "whichever the catalog
// happens to list first" is a different statement that was true only by luck.
export { DEFAULT_LLM_ENGINE };

/**
 * A runtime's display name, for every screen that shows one.
 *
 * One source, because there were two: the model step looked the label up and the
 * review screen printed the raw id, so the same deploy read "Served by vLLM" on
 * one screen and "Runtime: vllm" on the next — which reads like two different
 * things were configured. An id the catalog does not know is printed as itself:
 * the control plane's engine list can outrun this one, and an unrecognised
 * runtime is still worth showing.
 */
export function llmEngineLabel(engine: string): string {
  return LLM_ENGINES.find((e) => e.id === engine)?.label ?? engine;
}

/**
 * What the operator gets, said before they commit rather than discovered after.
 *
 * The version is described rather than numbered on purpose. The control plane
 * pins exactly one image per engine and the dashboard has no way to read that
 * pin; printing a number here would create a fifth place the version is written
 * down and a first place it can be wrong.
 */
export function managedSummary(kind: ResourceKind): { line: string; credentials: string } {
  const label = RESOURCE_KIND_LABELS[kind] ?? kind;
  if (kind === "s3") {
    return {
      line: `Provisioned from the official image, on a Storage server, reachable on the private mesh only.`,
      credentials:
        "An access key and secret are generated when the bucket comes up. They are shown once here — after that they live behind an audited reveal on the resource's Storage panel.",
    };
  }
  return {
    line: `Provisioned from the official ${label} image pinned by this control plane, tuned for the server type it lands on, and bound to the private mesh — never a public interface.`,
    credentials:
      "A user, password and database name are generated when the engine comes up. They are shown once here — after that they live behind an audited reveal on the resource's Database panel.",
  };
}

/**
 * A default name for a managed resource.
 *
 * `redis` alone collides the moment a second one is created in the same
 * environment, and the failure arrives from the control plane after the wizard
 * has closed. Suffixing by environment is what people type anyway.
 */
export function defaultManagedName(kind: ResourceKind, environmentName?: string): string {
  const base = kind === "s3" ? "storage" : kind;
  const env = environmentName?.trim().toLowerCase().replace(/[^a-z0-9-]+/g, "-");
  return env ? `${base}-${env}` : base;
}

/** Resource names travel into container names, DNS labels and volume names. */
export const RESOURCE_NAME_RE = /^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$/;

export function resourceNameError(name: string): string | null {
  const n = name.trim();
  if (!n) return "A name is required.";
  if (!RESOURCE_NAME_RE.test(n)) {
    return "Use lowercase letters, digits and dashes (up to 40 characters), starting and ending with a letter or digit.";
  }
  return null;
}
