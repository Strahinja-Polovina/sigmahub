import { describe, expect, it } from "vitest";
import { canSeeProject, effectiveProjectRole, roleAtLeast, roleRank } from "./rbac";

describe("role ranking", () => {
  it("orders the three tiers and fails closed on unknowns", () => {
    expect(roleRank("Org Admin")).toBeGreaterThan(roleRank("Project Admin"));
    expect(roleRank("Project Admin")).toBeGreaterThan(roleRank("Developer"));
    expect(roleRank("root")).toBe(0);
    expect(roleAtLeast("root", "Developer")).toBe(false);
    expect(roleAtLeast("Project Admin", "Developer")).toBe(true);
  });
});

describe("effectiveProjectRole", () => {
  it("org admins are never scoped", () => {
    expect(effectiveProjectRole("Org Admin", undefined, true)).toBe("Org Admin");
    expect(effectiveProjectRole("Org Admin", "Developer", true)).toBe("Org Admin");
  });

  it("zero grants = legacy org-wide access (nobody loses access on ship)", () => {
    expect(effectiveProjectRole("Project Admin", undefined, false)).toBe("Project Admin");
    expect(effectiveProjectRole("Developer", undefined, false)).toBe("Developer");
  });

  it("any grant scopes the user: ungranted projects become invisible", () => {
    expect(effectiveProjectRole("Project Admin", undefined, true)).toBeNull();
    expect(canSeeProject("Developer", undefined, true)).toBe(false);
  });

  it("the org role is the ceiling — a grant can only narrow, never widen", () => {
    // Org Developer granted Project Admin on a project stays Developer there.
    expect(effectiveProjectRole("Developer", "Project Admin", true)).toBe("Developer");
    // Org Project Admin granted Developer is narrowed to Developer.
    expect(effectiveProjectRole("Project Admin", "Developer", true)).toBe("Developer");
    // Matching grant passes through.
    expect(effectiveProjectRole("Project Admin", "Project Admin", true)).toBe("Project Admin");
  });
});
