/**
 * The build method, presented as a decision the user makes (SIGMA-209).
 *
 * Detection produces a verdict. The wizard's job is to SHOW that verdict with
 * its evidence and let the user disagree — because the previous behaviour on
 * disagreement was nothing: a repo with no Dockerfile and no compose file got
 * "not deployable", which is the worst dead end in the product and is reached by
 * the most ordinary repository shape there is.
 *
 * Every string here is derived from what the control plane detected. The
 * decision itself is the CP's (gitdetect sets `buildMethod`); this restates it
 * for a UI and defaults it for a CP too old to have sent one, so that a
 * dashboard talking to a lagging control plane still says something true.
 */

import type { DetectedComposeService } from "@/lib/deploy-spec";

export const BUILD_DOCKERFILE = "dockerfile";
export const BUILD_COMPOSE = "compose";
export const BUILD_NIXPACKS = "nixpacks";

export type BuildMethod =
  | typeof BUILD_DOCKERFILE
  | typeof BUILD_COMPOSE
  | typeof BUILD_NIXPACKS;

export const BUILD_METHOD_LABELS: Record<BuildMethod, string> = {
  [BUILD_DOCKERFILE]: "Dockerfile",
  [BUILD_COMPOSE]: "Docker Compose",
  [BUILD_NIXPACKS]: "Auto-build (Nixpacks)",
};

export const BUILD_METHOD_HINTS: Record<BuildMethod, string> = {
  [BUILD_DOCKERFILE]: "Build the image from a Dockerfile in the repository.",
  [BUILD_COMPOSE]: "Deploy every service the compose file declares, each as its own container.",
  [BUILD_NIXPACKS]:
    "No Dockerfile needed — the toolchain is derived from the project's own manifest.",
};

/** The detection result, as the wizard consumes it. Structurally the CP's
 *  `CpDetected` plus the SIGMA-209 fields, restated so this module stays free of
 *  server-only imports. */
export type DetectedRepo = {
  hasDockerfile?: boolean;
  hasCompose?: boolean;
  /** Relative to the build context — see gitdetect.Detected. */
  dockerfilePath?: string;
  /** Repo-relative. */
  composePath?: string;
  /** Directory the build runs in, relative to the repo root. */
  contextSubdir?: string;
  /** The CP's own verdict. Absent from a control plane older than SIGMA-209. */
  buildMethod?: string;
  language?: string;
  languageLabel?: string;
  ports?: number[];
  env?: string[];
  /** Always populated by the CP: a probe read from the repo, or a default TCP
   *  probe on the primary port. It pre-fills the networking step. */
  healthCheck?: {
    type?: string;
    path?: string;
    port?: number;
    intervalSec?: number;
    source?: string;
  };
  services?: DetectedComposeService[];
  deployable?: boolean;
  /** The repository could not be READ — a transport failure, a rate limit, an
   *  expired token. Different from "nothing here builds", and acted on
   *  differently: telling someone to commit a Dockerfile is wrong advice when
   *  the repo may already have one we could not see. */
  unreadable?: boolean;
  reason?: string;
};

export type BuildDecision = {
  /** True when the block is a read failure rather than a property of the repo,
   *  so the UI offers "try again" instead of "write a Dockerfile". */
  retryable?: boolean;
  /** null means nothing in this repository can be built — the only case that
   *  still blocks, and the only one that gets a starter Dockerfile offer. */
  method: BuildMethod | null;
  /** Whether the repository SAID how to build itself. An inferred method is
   *  still offered as the default; it is just labelled as a guess. */
  confident: boolean;
  /** One line naming the decision. */
  headline: string;
  /** The evidence behind it — the file we read, or the manifest we matched. */
  evidence: string;
  /** What happens as a consequence. */
  detail: string;
  /** Everything the user may switch to, always including `method`. Switching is
   *  always possible: detection is a best-effort read of someone else's
   *  repository, and being confidently wrong about it must not be terminal. */
  alternatives: BuildMethod[];
};

/** Methods a user can pick by hand, in the order they are offered. */
export const SWITCHABLE_METHODS: BuildMethod[] = [
  BUILD_DOCKERFILE,
  BUILD_COMPOSE,
  BUILD_NIXPACKS,
];

/**
 * What this repository will be built with, and why.
 *
 * The decision table, in order:
 *
 *   compose file present            → compose   (confident)
 *   Dockerfile present, no compose  → dockerfile(confident)
 *   neither, language recognized    → nixpacks  (confident — the manifest IS
 *                                                the evidence)
 *   neither, nothing recognized     → null      (blocked; offer a starter
 *                                                Dockerfile or a manual path)
 *
 * Compose beating a sibling Dockerfile is deliberate and load-bearing: the
 * compose file describes the WHOLE application, including the service that
 * Dockerfile builds. Preferring the Dockerfile would deploy one service of four
 * and report success — which is the shape of SIGMA-199.
 */
export function decideBuildMethod(d: DetectedRepo | null | undefined): BuildDecision {
  if (!d) {
    return {
      method: null,
      confident: false,
      headline: "Nothing detected yet",
      evidence: "No repository has been selected.",
      detail: "Pick a repository and sigmahub reads it.",
      alternatives: SWITCHABLE_METHODS,
    };
  }

  const where = d.contextSubdir ? ` in ${d.contextSubdir}` : "";

  if (d.hasCompose || d.buildMethod === BUILD_COMPOSE) {
    const n = d.services?.length ?? 0;
    return {
      method: BUILD_COMPOSE,
      confident: true,
      headline: "Deploying with Docker Compose",
      evidence: `Found ${d.composePath ?? "docker-compose.yml"}.`,
      detail: n
        ? `${n} ${n === 1 ? "service" : "services"} will be deployed, each as its own container. You choose where each one runs.`
        : "Each service in the file is deployed as its own container.",
      alternatives: SWITCHABLE_METHODS,
    };
  }

  if (d.hasDockerfile || d.buildMethod === BUILD_DOCKERFILE) {
    return {
      method: BUILD_DOCKERFILE,
      confident: true,
      headline: "Building from your Dockerfile",
      evidence: `Found ${d.dockerfilePath ?? "Dockerfile"}${where}.`,
      detail:
        "The image is built from this Dockerfile on every deploy. You can point at a different file or a different directory below.",
      alternatives: SWITCHABLE_METHODS,
    };
  }

  if (d.buildMethod === BUILD_NIXPACKS && d.language) {
    return {
      method: BUILD_NIXPACKS,
      confident: true,
      headline: `No Dockerfile — auto-building this ${d.languageLabel ?? d.language} app`,
      evidence: `Found a ${d.languageLabel ?? d.language} project manifest${where}.`,
      detail:
        "Nixpacks derives the toolchain and the start command from the project itself. Switch to a Dockerfile whenever you want control over the image.",
      alternatives: SWITCHABLE_METHODS,
    };
  }

  if (d.unreadable) {
    return {
      method: null,
      confident: false,
      headline: "Couldn't read this repository",
      evidence: d.reason ?? "The control plane could not reach GitHub.",
      // No starter Dockerfile here: the repository may already have one. The
      // action is to retry or fix access, not to commit a file.
      detail:
        "This is about access, not about your code — retry, or check that the GitHub App is still installed and has permission to read this repository.",
      alternatives: SWITCHABLE_METHODS,
      retryable: true,
    };
  }

  return {
    method: null,
    confident: false,
    headline: "This repository doesn't say how to build itself",
    evidence:
      d.reason ??
      "No Dockerfile, Compose file or recognizable project manifest was found.",
    detail:
      "Add a Dockerfile — sigmahub can write you a starter one — or point it at the subdirectory that holds your app.",
    alternatives: SWITCHABLE_METHODS,
  };
}

/** The build block persisted on the resource spec, or null for a compose app
 *  (whose services carry their own contexts) and for the plain default. */
export type BuildSpec = {
  method: BuildMethod;
  dockerfile?: string;
  contextSubdir?: string;
};

/**
 * Turn the chosen method plus the user's overrides into `spec.build`.
 *
 * This is the one assignment that carries the whole feature across the boundary
 * — the agent's image.build op has taken a dockerfile path and a context
 * subdirectory all along, and the single-container path never set either, which
 * is exactly why a monorepo could not be deployed. It is a pure function so the
 * wiring can be asserted (see build.test.ts and deploy-spec.test.ts), because
 * the last four regressions here were all "the fix was one argument and every
 * suite stayed green".
 */
export function buildSpecFor(input: {
  method: BuildMethod | null;
  dockerfile?: string;
  contextSubdir?: string;
}): BuildSpec | null {
  if (!input.method) return null;
  // A compose app's build instructions live per service, in spec.compose. A
  // top-level build block would name a context for an app that has several.
  if (input.method === BUILD_COMPOSE) return null;

  const spec: BuildSpec = { method: input.method };
  const dockerfile = input.dockerfile?.trim();
  const contextSubdir = normalizeSubdir(input.contextSubdir);
  // Nixpacks has no Dockerfile by definition; carrying a stale path from a
  // method the user switched away from would be a lie the agent then acts on.
  if (dockerfile && input.method === BUILD_DOCKERFILE) spec.dockerfile = dockerfile;
  if (contextSubdir) spec.contextSubdir = contextSubdir;
  return spec;
}

/**
 * A build context the agent will accept: repo-relative, no leading or trailing
 * slash, no traversal. The agent confines it too — this only means the user
 * hears about a bad path while they can still fix it.
 */
export function normalizeSubdir(value: string | null | undefined): string {
  const raw = (value ?? "").trim().replace(/^\.\//, "");
  if (!raw || raw === "." || raw === "/") return "";
  const parts = raw.split("/").filter((p) => p !== "" && p !== ".");
  if (parts.some((p) => p === "..")) return "";
  return parts.join("/");
}

/** Why a typed build context is not usable, or null. */
export function subdirError(value: string | null | undefined): string | null {
  const raw = (value ?? "").trim();
  if (!raw) return null;
  if (raw.startsWith("/")) return "Use a path relative to the repository root, without a leading slash.";
  if (raw.split("/").includes("..")) return "The build context has to stay inside the repository.";
  return null;
}

/**
 * A starter Dockerfile for a language we recognized but could not build.
 *
 * Deliberately plain and deliberately not clever: it is a file the user commits
 * and then owns. A multi-stage, cache-mount, distroless template would be a
 * better image and a worse starting point for someone who has just been told
 * their repository is not deployable.
 */
export function starterDockerfile(language: string | null | undefined, port = 3000): string {
  switch (language) {
    case "node":
      return [
        "FROM node:22-alpine",
        "WORKDIR /app",
        "COPY package*.json ./",
        "RUN npm ci --omit=dev",
        "COPY . .",
        `EXPOSE ${port}`,
        'CMD ["node", "server.js"]',
      ].join("\n");
    case "go":
      return [
        "FROM golang:1.24-alpine AS build",
        "WORKDIR /src",
        "COPY go.* ./",
        "RUN go mod download",
        "COPY . .",
        "RUN go build -o /app ./...",
        "",
        "FROM alpine:3.21",
        "COPY --from=build /app /app",
        `EXPOSE ${port}`,
        'CMD ["/app"]',
      ].join("\n");
    case "python":
      return [
        "FROM python:3.13-slim",
        "WORKDIR /app",
        "COPY requirements.txt ./",
        "RUN pip install --no-cache-dir -r requirements.txt",
        "COPY . .",
        `EXPOSE ${port}`,
        `CMD ["python", "-m", "gunicorn", "--bind", "0.0.0.0:${port}", "app:app"]`,
      ].join("\n");
    case "ruby":
      return [
        "FROM ruby:3.4-slim",
        "WORKDIR /app",
        "COPY Gemfile* ./",
        "RUN bundle install",
        "COPY . .",
        `EXPOSE ${port}`,
        `CMD ["bundle", "exec", "rackup", "--host", "0.0.0.0", "--port", "${port}"]`,
      ].join("\n");
    case "rust":
      return [
        "FROM rust:1.84 AS build",
        "WORKDIR /src",
        "COPY . .",
        "RUN cargo build --release",
        "",
        "FROM debian:bookworm-slim",
        "COPY --from=build /src/target/release/app /app",
        `EXPOSE ${port}`,
        'CMD ["/app"]',
      ].join("\n");
    default:
      // A language we have no template for still gets a scaffold rather than a
      // shrug: the shape of the answer is what the user is missing.
      return [
        "# Replace the base image and the start command with your own.",
        "FROM debian:bookworm-slim",
        "WORKDIR /app",
        "COPY . .",
        "# RUN <build your app>",
        `EXPOSE ${port}`,
        '# CMD ["./your-app"]',
      ].join("\n");
  }
}
