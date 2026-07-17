import { describe, expect, it } from "vitest";
import {
  environmentMirrorRow,
  localResourceKind,
  projectMirrorRow,
  resourceMirrorRow,
  resourceStatusText,
  slugifyName,
  staleIds,
} from "./mirror-sync";
import type { CpEnvironment, CpProject, CpResource } from "@/server/cp";

const cpResource = (over: Partial<CpResource> = {}): CpResource => ({
  id: "res_1",
  orgId: "org_1",
  projectId: "prj_1",
  environmentId: "env_1",
  serverId: "srv_1",
  name: "api",
  kind: "app",
  spec: {},
  status: {},
  createdAt: "2026-07-01T00:00:00Z",
  updatedAt: "2026-07-01T00:00:00Z",
  ...over,
});

describe("staleIds", () => {
  it("returns local ids the CP no longer owns", () => {
    expect(staleIds(["a", "b", "c"], ["b"])).toEqual(["a", "c"]);
  });
  it("tombstones everything when the CP owns nothing", () => {
    expect(staleIds(["a", "b"], [])).toEqual(["a", "b"]);
  });
  it("returns nothing when the mirror already matches", () => {
    expect(staleIds(["a"], ["a", "cp-only"])).toEqual([]);
  });
});

describe("resource mapping", () => {
  it("maps the CP kind vocabulary onto the local one", () => {
    expect(localResourceKind("mongodb")).toBe("mongo");
    expect(localResourceKind("postgres")).toBe("postgres");
  });

  it("prefers CP status.state, then the existing mirror value", () => {
    expect(resourceStatusText({ state: "running" }, "provisioning")).toBe("running");
    expect(resourceStatusText({}, "degraded")).toBe("degraded");
    expect(resourceStatusText(null, null)).toBe("provisioning");
    // Empty state string is "not populated yet", not a status.
    expect(resourceStatusText({ state: "" }, "running")).toBe("running");
  });

  it("builds a full local row, riding spec.repo/domain when present", () => {
    const row = resourceMirrorRow(
      cpResource({
        kind: "mongodb",
        serverId: "",
        spec: { repo: "acme/api", domain: "api.acme.dev" },
        status: { state: "running" },
      })
    );
    expect(row).toMatchObject({
      id: "res_1",
      kind: "mongo",
      serverId: null,
      status: "running",
      repo: "acme/api",
      domain: "api.acme.dev",
      version: "v1",
    });
    expect(row.lastDeployAt).toEqual(new Date("2026-07-01T00:00:00Z"));
  });

  it("keeps locally-held fields the CP spec does not carry", () => {
    const existing = {
      status: "running",
      repo: "acme/api",
      domain: "api.acme.dev",
      version: "v7",
      lastDeployAt: new Date("2026-07-10T12:00:00Z"),
    };
    const row = resourceMirrorRow(cpResource(), existing);
    expect(row.status).toBe("running");
    expect(row.repo).toBe("acme/api");
    expect(row.domain).toBe("api.acme.dev");
    expect(row.version).toBe("v7");
    expect(row.lastDeployAt).toEqual(existing.lastDeployAt);
  });
});

describe("project/environment mapping", () => {
  const cpProject: CpProject = {
    id: "prj_1",
    orgId: "org_1",
    name: "Payment API!",
    description: "d",
    createdBy: "u",
    createdAt: "2026-07-01T00:00:00Z",
  };

  it("generates the create-action slug for CP-only projects", () => {
    expect(slugifyName("Payment API!")).toBe("payment-api");
    expect(projectMirrorRow(cpProject).slug).toBe("payment-api");
  });

  it("keeps an existing slug so local links stay stable", () => {
    expect(projectMirrorRow(cpProject, { slug: "legacy-slug" }).slug).toBe(
      "legacy-slug"
    );
  });

  it("maps preview environments like any other env", () => {
    const env: CpEnvironment = {
      id: "env_pr7",
      orgId: "org_1",
      projectId: "prj_1",
      name: "pr-7",
      production: false,
      createdAt: "2026-07-02T00:00:00Z",
    };
    expect(environmentMirrorRow(env)).toEqual({
      id: "env_pr7",
      projectId: "prj_1",
      name: "pr-7",
      createdAt: new Date("2026-07-02T00:00:00Z"),
    });
  });
});
