import { describe, expect, it } from "vitest";
import {
  BUILD_COMPOSE,
  BUILD_DOCKERFILE,
  BUILD_NIXPACKS,
  buildSpecFor,
  decideBuildMethod,
  normalizeSubdir,
  starterDockerfile,
  subdirError,
} from "./build";

describe("the build-method decision table", () => {
  it("picks Compose when a compose file is present", () => {
    const d = decideBuildMethod({
      hasCompose: true,
      composePath: "docker-compose.yml",
      services: [{ name: "a", rollout: "blue-green" }],
      buildMethod: BUILD_COMPOSE,
    });
    expect(d.method).toBe(BUILD_COMPOSE);
    expect(d.confident).toBe(true);
    expect(d.evidence).toContain("docker-compose.yml");
  });

  // Compose beating a sibling Dockerfile is load-bearing: the compose file
  // describes the WHOLE application, including the service that Dockerfile
  // builds. Preferring the Dockerfile deploys one service of four and reports
  // success — the shape of SIGMA-199.
  it("prefers Compose over a sibling Dockerfile", () => {
    const d = decideBuildMethod({
      hasDockerfile: true,
      hasCompose: true,
      composePath: "compose.yaml",
      dockerfilePath: "Dockerfile",
    });
    expect(d.method).toBe(BUILD_COMPOSE);
  });

  it("picks the Dockerfile when there is no compose file", () => {
    const d = decideBuildMethod({ hasDockerfile: true, dockerfilePath: "Dockerfile" });
    expect(d.method).toBe(BUILD_DOCKERFILE);
    expect(d.confident).toBe(true);
  });

  it("names the subdirectory a monorepo's build was found in", () => {
    const d = decideBuildMethod({
      hasDockerfile: true,
      dockerfilePath: "Dockerfile",
      contextSubdir: "apps/api",
    });
    expect(d.evidence).toContain("apps/api");
  });

  // The dead end this replaces: "not deployable, go away", reached by the most
  // ordinary repository shape there is.
  it("falls back to Nixpacks when a language was recognized", () => {
    const d = decideBuildMethod({
      buildMethod: BUILD_NIXPACKS,
      language: "go",
      languageLabel: "Go",
      deployable: true,
    });
    expect(d.method).toBe(BUILD_NIXPACKS);
    expect(d.confident).toBe(true);
    expect(d.headline).toContain("Go");
  });

  it("blocks only when nothing at all was recognized, and says what to do", () => {
    const d = decideBuildMethod({ deployable: false, reason: "no Dockerfile found" });
    expect(d.method).toBeNull();
    expect(d.detail).toContain("starter");
  });

  it("always offers every method as an alternative, however confident it is", () => {
    for (const detected of [
      { hasCompose: true },
      { hasDockerfile: true },
      { buildMethod: BUILD_NIXPACKS, language: "node" },
      { deployable: false },
    ]) {
      expect(decideBuildMethod(detected).alternatives).toEqual([
        BUILD_DOCKERFILE,
        BUILD_COMPOSE,
        BUILD_NIXPACKS,
      ]);
    }
  });
});

describe("what the build decision sends to the control plane", () => {
  // This is the one assignment that carries the whole feature across the
  // boundary: the agent's image.build op has taken a dockerfile path and a
  // context subdirectory all along, and the single-container path never set
  // either, which is why a monorepo could not be deployed.
  it("carries the Dockerfile path and the build context", () => {
    expect(
      buildSpecFor({ method: BUILD_DOCKERFILE, dockerfile: "Dockerfile", contextSubdir: "apps/api" })
    ).toEqual({ method: "dockerfile", dockerfile: "Dockerfile", contextSubdir: "apps/api" });
  });

  it("carries the builder for an auto-build", () => {
    expect(buildSpecFor({ method: BUILD_NIXPACKS, contextSubdir: "services/api" })).toEqual({
      method: "nixpacks",
      contextSubdir: "services/api",
    });
  });

  // A nixpacks build has no Dockerfile by definition. Carrying a stale path
  // from a method the user switched away from is a lie the agent acts on.
  it("drops a Dockerfile path when the method is not Dockerfile", () => {
    expect(buildSpecFor({ method: BUILD_NIXPACKS, dockerfile: "Dockerfile" })).toEqual({
      method: "nixpacks",
    });
  });

  // A Compose app's build instructions live per service in spec.compose; a
  // top-level build block would name one context for an app that has several.
  it("writes no build block for a Compose app", () => {
    expect(buildSpecFor({ method: BUILD_COMPOSE, contextSubdir: "x" })).toBeNull();
  });

  it("writes nothing when nothing was decided", () => {
    expect(buildSpecFor({ method: null })).toBeNull();
  });
});

describe("build contexts stay inside the repository", () => {
  it("normalizes the ways people write the root", () => {
    for (const raw of ["", " ", ".", "/", "./"]) {
      expect(normalizeSubdir(raw)).toBe("");
    }
  });

  it("strips decoration but keeps the path", () => {
    expect(normalizeSubdir("./apps/api/")).toBe("apps/api");
    expect(normalizeSubdir("apps//api")).toBe("apps/api");
  });

  it("refuses traversal, and says so before the agent has to", () => {
    expect(normalizeSubdir("../etc")).toBe("");
    expect(subdirError("../etc")).toContain("inside the repository");
    expect(subdirError("/abs/path")).toContain("relative");
    expect(subdirError("apps/api")).toBeNull();
  });
});

describe("the starter Dockerfile", () => {
  it("uses the language we detected", () => {
    expect(starterDockerfile("go")).toContain("FROM golang");
    expect(starterDockerfile("node")).toContain("FROM node");
    expect(starterDockerfile("python")).toContain("FROM python");
  });

  it("exposes the port the wizard is about to configure", () => {
    expect(starterDockerfile("node", 8080)).toContain("EXPOSE 8080");
  });

  // A language we have no template for still gets a scaffold rather than a
  // shrug: the SHAPE of the answer is what the user is missing.
  it("still produces something for an unknown language", () => {
    const out = starterDockerfile("brainfuck");
    expect(out).toContain("FROM ");
    expect(out).toContain("EXPOSE ");
  });
});

// A repository we could not READ is not a repository with nothing to build.
//
// detectRepo never throws — it catches and returns a synthetic failure — so a
// 500, a rate limit or an expired token arrived in the same branch as "no
// Dockerfile here". The wizard then told the user their repository doesn't say
// how to build itself and offered to write them a Dockerfile, for a repo that
// may already have one the control plane simply could not see. The reason was
// wrong and the action was destructive advice.
describe("an unreadable repository", () => {
  const unreadable = decideBuildMethod({
    deployable: false,
    unreadable: true,
    reason: "GitHub returned 502",
  });

  it("blames access, not the code", () => {
    expect(unreadable.headline).toMatch(/read/i);
    expect(unreadable.headline).not.toMatch(/build itself/i);
    expect(unreadable.evidence).toContain("502");
  });

  it("offers a retry instead of a starter Dockerfile", () => {
    expect(unreadable.retryable).toBe(true);
    expect(unreadable.detail).not.toMatch(/starter|write you|add a dockerfile/i);
  });

  it("still blocks, and still lets the user pick a method by hand", () => {
    expect(unreadable.method).toBeNull();
    expect(unreadable.alternatives.length).toBeGreaterThan(0);
  });

  it("leaves the genuinely undeployable case alone", () => {
    const nothing = decideBuildMethod({ deployable: false });
    expect(nothing.headline).toMatch(/build itself/i);
    expect(nothing.retryable).toBeFalsy();
  });
});
