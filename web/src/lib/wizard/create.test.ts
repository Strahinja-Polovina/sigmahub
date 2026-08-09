import { describe, expect, it } from "vitest";
import { createResourceInput, createSecretsFor, shouldWireRepo, type WizardDraftState } from "./create";
import { BUILD_COMPOSE, BUILD_DOCKERFILE, BUILD_NIXPACKS } from "./build";
import { ROLLOUT_BLUE_GREEN, ROLLOUT_RECREATE } from "@/lib/deploy-spec";

/**
 * These are the assertions the last four regressions in this flow needed and
 * did not have. Each one covers a single statement that, when deleted, leaves
 * every other suite green while the product silently stops doing the thing.
 */
const base: WizardDraftState = {
  kind: "app",
  name: "storefront",
  projectId: "prj_1",
  environmentId: "env_1",
  serverId: "srv_1",
  clusterId: "",
  domain: "",
  repo: { fullName: "acme/storefront", installationId: "42" },
  branch: "main",
  detected: null,
  method: BUILD_DOCKERFILE,
  dockerfile: "Dockerfile",
  contextSubdir: "",
  ports: [],
  healthPath: "/",
  s3Engine: "minio",
  llmEngine: "vllm",
  llmModel: "",
  envVars: [],
};

describe("an application built from a Dockerfile", () => {
  const args = createResourceInput({
    ...base,
    contextSubdir: "apps/api",
    detected: {
      hasDockerfile: true,
      dockerfilePath: "Dockerfile",
      contextSubdir: "apps/api",
      ports: [8080],
      healthCheck: { type: "http", path: "/livez", port: 8080, intervalSec: 30 },
    },
    ports: [{ id: "1", container: 8080, host: 0 }],
    healthPath: "/livez",
    domain: " shop.acme.com ",
  });

  it("sends the repository and the installation it was picked from", () => {
    expect(args.repo).toBe("acme/storefront");
    expect(args.installationId).toBe("42");
  });

  // SIGMA-209: the agent's build op has taken both fields all along.
  it("sends the build decision", () => {
    expect(args.build).toEqual({
      method: "dockerfile",
      dockerfile: "Dockerfile",
      contextSubdir: "apps/api",
    });
  });

  // SIGMA-210: the mappings the user left the networking step with, not the raw
  // detected list they were shown so they could change it.
  it("sends the wizard's port mappings", () => {
    expect(args.ports).toEqual([{ container: 8080, host: 0, protocol: "tcp" }]);
  });

  // SIGMA-160: without a probe the rollout's health gate targets nothing.
  it("sends the confirmed health check, keeping the repository's interval", () => {
    expect(args.detected?.healthCheck).toEqual({
      type: "http",
      path: "/livez",
      port: 8080,
      intervalSec: 30,
    });
  });

  it("trims the domain", () => {
    expect(args.domain).toBe("shop.acme.com");
  });
});

describe("an application deployed from Compose", () => {
  const services = [
    { name: "api", build: ".", ports: [8080], rollout: ROLLOUT_BLUE_GREEN },
    { name: "db", image: "postgres:16", namedVolumes: ["pgdata"], rollout: ROLLOUT_RECREATE },
  ];
  const args = createResourceInput({
    ...base,
    method: BUILD_COMPOSE,
    detected: {
      hasCompose: true,
      composePath: "docker-compose.yml",
      ports: [8080],
      healthCheck: { type: "http", path: "/health", port: 8080 },
      services,
    },
    ports: [{ id: "1", container: 8080, host: 0 }],
  });

  // SIGMA-199: dropping the graph made a repo describing five services deploy
  // as one container, and made the reconciler's per-service branch unreachable.
  it("sends the whole service graph", () => {
    expect(args.detected?.services, "a compose app with no graph deploys as ONE container").toEqual(
      services
    );
  });

  // Its build instructions are per service; one top-level context would name a
  // single directory for an app that has several.
  it("sends no top-level build block", () => {
    expect(args.build).toBeUndefined();
  });

  // Its ports belong to its services.
  it("sends no top-level port list", () => {
    expect(args.ports).toBeUndefined();
  });

  it("keeps the probe the compose file declared", () => {
    expect(args.detected?.healthCheck).toEqual({
      type: "http",
      path: "/health",
      port: 8080,
      intervalSec: undefined,
    });
  });
});

describe("an application with no Dockerfile at all", () => {
  const args = createResourceInput({
    ...base,
    method: BUILD_NIXPACKS,
    dockerfile: "",
    contextSubdir: "services/api",
    detected: {
      buildMethod: "nixpacks",
      language: "go",
      ports: [8080],
      healthCheck: { type: "tcp", port: 8080 },
    },
    ports: [{ id: "1", container: 8080, host: 0 }],
    healthPath: "",
  });

  it("names the builder, so the agent does not look for a Dockerfile", () => {
    expect(args.build).toEqual({ method: "nixpacks", contextSubdir: "services/api" });
  });

  it("falls back to a TCP probe when the path was cleared", () => {
    expect(args.detected?.healthCheck).toEqual({ type: "tcp", port: 8080 });
  });
});

describe("the deploy target", () => {
  // SIGMA-200: the cluster id could be dropped with everything green because
  // nothing on the web side asserted what goes on the wire.
  it("sends a cluster and no server", () => {
    const args = createResourceInput({ ...base, serverId: "", clusterId: "cls_1" });
    expect(args.clusterId).toBe("cls_1");
    expect(args.serverId).toBeUndefined();
  });

  it("sends a server and no cluster", () => {
    const args = createResourceInput(base);
    expect(args.serverId).toBe("srv_1");
    expect(args.clusterId).toBeUndefined();
  });
});

describe("managed kinds send nothing about a repository", () => {
  const managed: WizardDraftState = {
    ...base,
    kind: "redis",
    name: "cache-production",
    // Deliberately left populated: a kind change must not smuggle the previous
    // path's answers into the request.
    method: BUILD_DOCKERFILE,
    detected: { hasDockerfile: true, ports: [3000] },
    ports: [{ id: "1", container: 3000, host: 0 }],
  };

  it("omits the repository, the build and the ports", () => {
    const args = createResourceInput(managed);
    expect(args.repo).toBeUndefined();
    expect(args.installationId).toBeUndefined();
    expect(args.build).toBeUndefined();
    expect(args.ports).toBeUndefined();
    expect(args.detected).toBeUndefined();
  });

  it("sends the object-storage engine", () => {
    expect(createResourceInput({ ...managed, kind: "s3", s3Engine: "seaweedfs" }).s3Engine).toBe(
      "seaweedfs"
    );
  });

  it("sends the inference runtime and model", () => {
    expect(
      createResourceInput({ ...managed, kind: "llm", llmModel: "  meta-llama/Llama-3.1-8B  " }).llm
    ).toEqual({ engine: "vllm", model: "meta-llama/Llama-3.1-8B" });
  });
});

describe("what happens after the resource exists", () => {
  const withVars: WizardDraftState = {
    ...base,
    envVars: [
      { id: "1", key: " DATABASE_URL ", value: "postgres://x", secret: true },
      { id: "2", key: "EMPTY", value: "", secret: false },
      { id: "3", key: "", value: "", secret: false },
    ],
  };

  it("creates a secret only for a key with a value", () => {
    expect(createSecretsFor(withVars)).toEqual([
      { key: "DATABASE_URL", value: "postgres://x" },
    ]);
  });

  // A managed engine generates its own credentials; the flow never collected
  // variables for it, and writing any would be writing the user's leftovers
  // from a path they abandoned.
  it("creates none for a managed engine", () => {
    expect(createSecretsFor({ ...withVars, kind: "postgres" })).toEqual([]);
  });

  it("wires push-to-deploy only for a repo-backed app against a control plane", () => {
    expect(shouldWireRepo(base, true)).toBe(true);
    expect(shouldWireRepo(base, false), "demo mode has no webhook receiver").toBe(false);
    expect(shouldWireRepo({ ...base, kind: "redis" }, true)).toBe(false);
    expect(shouldWireRepo({ ...base, repo: null }, true)).toBe(false);
  });
});
