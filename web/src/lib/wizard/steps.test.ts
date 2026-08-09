import { describe, expect, it } from "vitest";
import {
  RESOURCE_CATEGORIES,
  RESOURCE_CATEGORY_CATALOG,
  RESOURCE_KINDS,
  categoryForKind,
  type ResourceKind,
} from "@/lib/server-catalog.generated";
import {
  decisionCount,
  hasStep,
  kindPickerPhase,
  nextStepId,
  pickCategory,
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

  // Categories went in FRONT of the kinds on step 1, and the one thing that
  // could not survive that is the count: a category rendered as its own step id
  // would add a screen to every flow. Stated per kind rather than as an
  // inequality, so a flow that grows one is named by the failure.
  it("costs no kind a decision to have gained a category", () => {
    // Keyed on ResourceKind so a kind added to the control plane's catalog
    // cannot reach this file without someone stating what it costs to deploy.
    const expected: Record<ResourceKind, number> = {
      app: 5, // source, build, network, target, variables
      postgres: 2,
      mysql: 2,
      mongodb: 2,
      redis: 2,
      s3: 2,
      llm: 2,
    };
    for (const kind of RESOURCE_KINDS) {
      expect(decisionCount(kind), kind).toBe(expected[kind]);
    }
    // And the picker itself is still one step, not two.
    expect(stepsForKind(null).map((s) => s.id)).toEqual(["kind"]);
  });
});

// Step 1 asks for a category and then, only when the category holds more than
// one kind, for the kind inside it. Both faces are the SAME step: the sequence
// above is what a category id would have grown.
describe("step 1 picks a category, then the kinds inside it", () => {
  // The decision this screen exists to make, and the one it must not undo: a
  // question with a single possible answer is not asked.
  it("resolves a category holding one kind straight through to it", () => {
    for (const id of RESOURCE_CATEGORIES) {
      const { kinds } = RESOURCE_CATEGORY_CATALOG[id];
      if (kinds.length !== 1) continue;
      expect(pickCategory(id), id).toEqual({ category: id, kind: kinds[0] });
      // …and it never opened a list, so the picker is still on the categories.
      expect(kindPickerPhase(id), id).toBe("categories");
    }
  });

  it("shows the kinds of a category holding more than one, choosing none of them", () => {
    expect(pickCategory("database")).toEqual({ category: "database", kind: null });
    expect(kindPickerPhase("database")).toBe("kinds");
    expect(RESOURCE_CATEGORY_CATALOG.database.kinds.length).toBeGreaterThan(1);
  });

  it("offers every category before one is picked", () => {
    expect(kindPickerPhase(null)).toBe("categories");
    expect(pickCategory(null)).toEqual({ category: null, kind: null });
  });

  // Application holds one kind TODAY. The structure is what is being asserted:
  // whichever categories hold one, resolve; whichever hold several, list.
  it("puts every kind inside exactly one category the picker can reach", () => {
    const reachable = RESOURCE_CATEGORIES.flatMap((id) => RESOURCE_CATEGORY_CATALOG[id].kinds);
    expect([...reachable].sort()).toEqual([...RESOURCE_KINDS].sort());
    for (const kind of RESOURCE_KINDS) {
      const category = categoryForKind(kind);
      expect(category, kind).not.toBeNull();
      expect(RESOURCE_CATEGORY_CATALOG[category!].kinds, kind).toContain(kind);
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

  // Changing CATEGORY is the same event: what resolves the step is the kind
  // pickCategory settles on, which is null while a category's list is open.
  it("cannot strand a half-configured Redis on the application's Build screen", () => {
    // Standing on Target with a Redis, the user goes back to step 1 and opens
    // Application. Its single kind resolves, and the app flow has no engine
    // step to hold the target they had picked.
    const application = pickCategory("application");
    expect(resolveStep(application.kind, "engine")).toBe("kind");
    // The other direction: an app on Build, switching to the Database list.
    const database = pickCategory("database");
    expect(database.kind).toBeNull();
    expect(resolveStep(database.kind, "build")).toBe("kind");
    // And backing out of a category leaves nothing standing either.
    expect(resolveStep(pickCategory(null).kind, "source")).toBe("kind");
  });

  it("keeps a step the resolved kind still has", () => {
    expect(resolveStep(pickCategory("storage").kind, "target")).toBe("target");
  });
});
