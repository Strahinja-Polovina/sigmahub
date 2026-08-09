import { describe, expect, it } from "vitest";
import { RESOURCE_KINDS } from "@/lib/server-catalog.generated";
import {
  decisionCount,
  hasStep,
  nextStepId,
  prevStepId,
  resolveStep,
  stepsForKind,
} from "./steps";

describe("step sequences are per type", () => {
  it("shows only the type picker before a type is chosen", () => {
    expect(stepsForKind(null).map((s) => s.id)).toEqual(["kind"]);
  });

  it("gives an application every screen it needs", () => {
    expect(stepsForKind("app").map((s) => s.id)).toEqual([
      "kind",
      "source",
      "build",
      "networking",
      "target",
      "env",
      "review",
      "create",
    ]);
  });

  // SIGMA-212 states the requirement as a number, and a number nothing counts
  // is a number that drifts: a managed Redis walked FIVE screens to make two
  // decisions, two of which were about a git repository it does not have.
  it("keeps every managed kind to two decisions beyond the type", () => {
    for (const kind of ["postgres", "mysql", "mongodb", "redis", "s3", "llm"] as const) {
      expect(decisionCount(kind), kind).toBeLessThanOrEqual(2);
    }
  });

  it("never asks a managed kind about a repository, a build or variables", () => {
    for (const kind of ["postgres", "mysql", "mongodb", "redis", "s3"] as const) {
      for (const step of ["source", "build", "env", "networking"] as const) {
        expect(hasStep(kind, step), `${kind} must not have a ${step} step`).toBe(false);
      }
    }
  });

  it("has a flow for every kind the catalog knows", () => {
    for (const kind of RESOURCE_KINDS) {
      const steps = stepsForKind(kind);
      expect(steps[0].id, kind).toBe("kind");
      expect(steps[steps.length - 1].id, kind).toBe("create");
    }
  });
});

describe("movement", () => {
  it("walks an application forwards and back through its own steps", () => {
    expect(nextStepId("app", "source")).toBe("build");
    expect(prevStepId("app", "build")).toBe("source");
  });

  // The old wizard tracked a NUMBER and special-cased "if this is a managed
  // service, step 1 goes to step 3" — which is how a managed service could
  // reach the Build screen by pressing Back.
  it("skips nothing for a database because there is nothing to skip", () => {
    expect(nextStepId("redis", "kind")).toBe("engine");
    expect(nextStepId("redis", "engine")).toBe("target");
    expect(nextStepId("redis", "target")).toBe("create");
    expect(prevStepId("redis", "target")).toBe("engine");
    expect(prevStepId("redis", "kind")).toBeNull();
  });

  it("stops at the ends", () => {
    expect(nextStepId("app", "create")).toBeNull();
    expect(prevStepId("app", "kind")).toBeNull();
  });
});

describe("changing type mid-flow", () => {
  // Picking a different type while standing on step 4 used to leave the step
  // number alone, so a Redis could be standing on the application flow's Build
  // screen — which renders nothing for it, because it has no repo.
  it("sends you back to the picker when the new type has no such step", () => {
    expect(resolveStep("redis", "build")).toBe("kind");
    expect(resolveStep("redis", "source")).toBe("kind");
  });

  it("keeps a step both types share", () => {
    expect(resolveStep("redis", "target")).toBe("target");
    expect(resolveStep("app", "target")).toBe("target");
  });
});
