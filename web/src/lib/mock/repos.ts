/**
 * The repositories demo mode inspects (SIGMA-215).
 *
 * Demo mode has no control plane, so nothing can read a real repository — these
 * stand in for the inspector's output. They are DATA rather than JSX because
 * every path the wizard can take has to be walkable offline, and "is there a
 * fixture that reaches the nixpacks screen" is a question a test should be able
 * to answer. A demo where every repo is a single container is a demo of the bug
 * this flow was built to remove.
 *
 * The set is chosen to cover the build-method decision table end to end:
 *
 *   acme/storefront      Dockerfile at the root      → dockerfile
 *   acme/api             compose, 4 services         → compose
 *   acme/edge-cache      compose with a fixed host   → compose + the ignored
 *                        port and a named volume       binding + recreate notices
 *   acme/ml-inference    Dockerfile, CUDA base       → dockerfile, GPU target
 *   acme/reporting       go.mod, no Dockerfile       → nixpacks
 *   acme/platform        nothing at the root, an     → dockerfile in a subdir
 *                        app under apps/api
 */

import {
  ROLLOUT_BLUE_GREEN,
  ROLLOUT_RECREATE,
  type DetectedComposeService,
} from "@/lib/deploy-spec";
import {
  BUILD_COMPOSE,
  BUILD_DOCKERFILE,
  BUILD_NIXPACKS,
  type DetectedRepo,
} from "@/lib/wizard/build";

/** A repository the demo picker offers, plus the detection it stands for. */
export type MockRepo = {
  fullName: string;
  description: string;
  private: boolean;
  defaultBranch: string;
  /** Branches the branch picker offers. The default is always first. */
  branches: string[];
  detected: DetectedRepo & { services?: DetectedComposeService[] };
};

export const MOCK_REPOS: MockRepo[] = [
  {
    fullName: "acme/storefront",
    description: "Next.js customer-facing storefront",
    private: true,
    defaultBranch: "main",
    branches: ["main", "next", "hotfix/checkout"],
    detected: {
      hasDockerfile: true,
      hasCompose: false,
      buildMethod: BUILD_DOCKERFILE,
      dockerfilePath: "Dockerfile",
      ports: [3000],
      env: ["DATABASE_URL", "NEXT_PUBLIC_API_URL", "SESSION_SECRET", "STRIPE_API_KEY"],
      healthCheck: { type: "http", path: "/healthz", port: 3000, intervalSec: 10, source: "dockerfile" },
      deployable: true,
    },
  },
  {
    fullName: "acme/api",
    description: "Core REST + gRPC services",
    private: true,
    defaultBranch: "main",
    branches: ["main", "develop"],
    detected: {
      hasDockerfile: true,
      hasCompose: true,
      buildMethod: BUILD_COMPOSE,
      composePath: "docker-compose.yml",
      dockerfilePath: "Dockerfile",
      ports: [8080],
      env: ["DATABASE_URL", "REDIS_URL", "JWT_SIGNING_KEY", "LOG_LEVEL"],
      healthCheck: { type: "http", path: "/health", port: 8080, intervalSec: 10, source: "compose" },
      deployable: true,
      services: [
        {
          name: "api",
          build: ".",
          dockerfile: "Dockerfile",
          ports: [8080],
          dependsOn: ["db", "cache"],
          rollout: ROLLOUT_BLUE_GREEN,
        },
        { name: "worker", build: "./worker", rollout: ROLLOUT_BLUE_GREEN },
        { name: "cache", image: "redis:7.4", ports: [6379], rollout: ROLLOUT_BLUE_GREEN },
        {
          name: "db",
          image: "postgres:16",
          ports: [5432],
          namedVolumes: ["pgdata"],
          rollout: ROLLOUT_RECREATE,
        },
      ],
    },
  },
  {
    fullName: "acme/edge-cache",
    description: "Redis-backed edge cache",
    private: false,
    defaultBranch: "main",
    branches: ["main"],
    // Carries a published host port and a named volume, which are the only way
    // the ignored-binding and recreate notices are reachable offline.
    detected: {
      hasDockerfile: false,
      hasCompose: true,
      buildMethod: BUILD_COMPOSE,
      composePath: "compose.yaml",
      ports: [6379],
      env: ["CACHE_TTL_SECONDS"],
      healthCheck: { type: "tcp", port: 6379, intervalSec: 10, source: "default" },
      deployable: true,
      services: [
        {
          name: "cache",
          image: "redis:7.4",
          ports: [6379],
          publishedPorts: [6379],
          namedVolumes: ["cachedata"],
          rollout: ROLLOUT_RECREATE,
        },
        { name: "warmer", build: "./warmer", dependsOn: ["cache"], rollout: ROLLOUT_BLUE_GREEN },
      ],
    },
  },
  {
    fullName: "acme/ml-inference",
    description: "vLLM inference server",
    private: true,
    defaultBranch: "main",
    branches: ["main", "cuda-12.6"],
    detected: {
      hasDockerfile: true,
      hasCompose: false,
      buildMethod: BUILD_DOCKERFILE,
      dockerfilePath: "Dockerfile",
      ports: [8000],
      env: ["HUGGING_FACE_HUB_TOKEN", "MODEL_ID"],
      healthCheck: { type: "http", path: "/health", port: 8000, intervalSec: 10, source: "dockerfile" },
      deployable: true,
    },
  },
  {
    // The repo that used to be a dead end: a real service that never had a
    // reason to containerize itself.
    fullName: "acme/reporting",
    description: "Nightly reporting service — no Dockerfile",
    private: true,
    defaultBranch: "main",
    branches: ["main", "wip/csv-export"],
    detected: {
      hasDockerfile: false,
      hasCompose: false,
      buildMethod: BUILD_NIXPACKS,
      language: "go",
      languageLabel: "Go",
      ports: [8080],
      env: ["REPORT_BUCKET", "SMTP_PASSWORD"],
      healthCheck: { type: "tcp", port: 8080, intervalSec: 10, source: "default" },
      deployable: true,
    },
  },
  {
    // The monorepo: a root that describes nothing, an app two levels down.
    fullName: "acme/platform",
    description: "Monorepo — apps/api, apps/web, packages/*",
    private: true,
    defaultBranch: "main",
    branches: ["main", "release/2026-08"],
    detected: {
      hasDockerfile: true,
      hasCompose: false,
      buildMethod: BUILD_DOCKERFILE,
      dockerfilePath: "Dockerfile",
      contextSubdir: "apps/api",
      ports: [8080],
      env: ["DATABASE_URL", "OTEL_EXPORTER_OTLP_ENDPOINT"],
      healthCheck: { type: "http", path: "/livez", port: 8080, intervalSec: 10, source: "dockerfile" },
      deployable: true,
    },
  },
];

export function findMockRepo(fullName: string): MockRepo | undefined {
  return MOCK_REPOS.find((r) => r.fullName === fullName);
}

export function searchMockRepos(query: string): MockRepo[] {
  const q = query.trim().toLowerCase();
  if (!q) return MOCK_REPOS;
  return MOCK_REPOS.filter(
    (r) => r.fullName.toLowerCase().includes(q) || r.description.toLowerCase().includes(q)
  );
}
