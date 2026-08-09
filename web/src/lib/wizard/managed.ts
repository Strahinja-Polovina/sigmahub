/**
 * The managed-data path: databases and object storage (SIGMA-212).
 *
 * A managed engine has no repository, no build, no ports to map and no
 * variables to set — the image is the control plane's, the port is the
 * engine's, and the credentials are generated. Everything the old wizard asked
 * for on the way here was about a git repo it does not have, which is why a
 * Redis took five screens to make two decisions.
 */

import { RESOURCE_KIND_LABELS, type ResourceKind } from "@/lib/server-catalog.generated";

export const DATABASE_KINDS: ResourceKind[] = ["postgres", "mysql", "mongodb", "redis"];

export function isDatabaseKind(kind: ResourceKind | null | undefined): boolean {
  return !!kind && DATABASE_KINDS.includes(kind);
}

/** Kinds that need no repository, no build and no variables. */
export function isManagedKind(kind: ResourceKind | null | undefined): boolean {
  return isDatabaseKind(kind) || kind === "s3";
}

/**
 * Object-storage engines the control plane can provision.
 *
 * An unknown engine is refused at create (store.CreateResource), so this list is
 * the contract rather than free text — the same reason the LLM runtimes below
 * are enumerated instead of typed. The VERSIONS are deliberately absent: the CP
 * pins one image per engine, so offering a version picker would be offering a
 * choice it ignores.
 */
export const S3_ENGINES = [
  {
    id: "minio",
    label: "MinIO",
    detail: "The default. Widely deployed, strict S3 API compatibility.",
  },
  {
    id: "seaweedfs",
    label: "SeaweedFS",
    detail: "Lighter on memory for very large object counts.",
  },
] as const;

export const DEFAULT_S3_ENGINE = S3_ENGINES[0].id;

/** Inference runtimes the control plane knows how to render. */
export const LLM_ENGINES = [
  { id: "vllm", label: "vLLM", detail: "High-throughput serving for most open models" },
  { id: "ollama", label: "Ollama", detail: "Simple single-model serving" },
] as const;

export const DEFAULT_LLM_ENGINE = LLM_ENGINES[0].id;

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
