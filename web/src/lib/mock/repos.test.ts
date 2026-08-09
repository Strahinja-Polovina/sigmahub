import { describe, expect, it } from "vitest";
import { MOCK_REPOS, findMockRepo, searchMockRepos } from "./repos";
import {
  BUILD_COMPOSE,
  BUILD_DOCKERFILE,
  BUILD_NIXPACKS,
  decideBuildMethod,
} from "@/lib/wizard/build";
import { ignoredHostPorts, recreateSummary } from "@/lib/deploy-spec";

/**
 * Demo mode has to walk EVERY path, and "is there a fixture that reaches the
 * nixpacks screen" is a question that must be answerable without opening the
 * app. A demo where every repo is a single container is a demo of the bug this
 * flow was built to remove (SIGMA-215).
 */
describe("the demo fixtures cover the whole build-method decision table", () => {
  it("reaches every branch of it", () => {
    const methods = new Set(MOCK_REPOS.map((r) => decideBuildMethod(r.detected).method));
    expect(methods).toEqual(new Set([BUILD_DOCKERFILE, BUILD_COMPOSE, BUILD_NIXPACKS]));
  });

  it("has a repo with neither a Dockerfile nor a compose file", () => {
    const repo = findMockRepo("acme/reporting");
    expect(repo?.detected.hasDockerfile).toBe(false);
    expect(repo?.detected.hasCompose).toBe(false);
    expect(decideBuildMethod(repo!.detected).method).toBe(BUILD_NIXPACKS);
  });

  it("has a monorepo whose build lives below the root", () => {
    const repo = findMockRepo("acme/platform");
    expect(repo?.detected.contextSubdir).toBe("apps/api");
  });

  // A repo the demo presents as Compose has to CARRY a graph, or demo mode
  // shows the one shape SIGMA-199 exists to prevent.
  it("has compose repos that carry real service graphs", () => {
    const compose = MOCK_REPOS.filter((r) => r.detected.hasCompose);
    expect(compose.length).toBeGreaterThan(0);
    for (const repo of compose) {
      expect(repo.detected.services?.length ?? 0, repo.fullName).toBeGreaterThan(1);
    }
  });

  // The recreate and ignored-binding notices are only reachable offline if some
  // fixture carries the evidence behind them.
  it("reaches the recreate and ignored-host-port notices", () => {
    const all = MOCK_REPOS.flatMap((r) => r.detected.services ?? []);
    expect(recreateSummary(all).length).toBeGreaterThan(0);
    expect(ignoredHostPorts(all).length).toBeGreaterThan(0);
  });

  it("gives every repo a health check and at least one port, so the network step is real", () => {
    for (const repo of MOCK_REPOS) {
      expect(repo.detected.ports?.length ?? 0, repo.fullName).toBeGreaterThan(0);
      expect(repo.detected.healthCheck?.type, repo.fullName).toBeTruthy();
    }
  });

  it("gives every repo variable keys, so the Variables step is seeded offline", () => {
    for (const repo of MOCK_REPOS) {
      expect(repo.detected.env?.length ?? 0, repo.fullName).toBeGreaterThan(0);
    }
  });

  it("offers branches with the default among them", () => {
    for (const repo of MOCK_REPOS) {
      expect(repo.branches, repo.fullName).toContain(repo.defaultBranch);
      expect(repo.branches[0], repo.fullName).toBe(repo.defaultBranch);
    }
  });
});

describe("the demo picker", () => {
  it("filters by name and by description", () => {
    expect(searchMockRepos("platform").map((r) => r.fullName)).toEqual(["acme/platform"]);
    expect(searchMockRepos("monorepo").map((r) => r.fullName)).toEqual(["acme/platform"]);
    expect(searchMockRepos("")).toHaveLength(MOCK_REPOS.length);
  });

  it("finds nothing rather than guessing", () => {
    expect(searchMockRepos("zzz")).toEqual([]);
    expect(findMockRepo("nope/nope")).toBeUndefined();
  });
});
