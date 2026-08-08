import { describe, expect, it } from "vitest";
import {
  environmentMirrorRow,
  localDeployStatus,
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
  // There is no kind translation any more: the mirror stores the control
  // plane's vocabulary verbatim (SIGMA-198). The pair of opposite-facing
  // translators this replaced were also routinely bypassed — three call sites
  // accepted both spellings rather than trust either one.
  it("stores the CP kind verbatim", () => {
    expect(resourceMirrorRow(cpResource({ kind: "mongodb" })).kind).toBe("mongodb");
    expect(resourceMirrorRow(cpResource({ kind: "postgres" })).kind).toBe("postgres");
  });

  it("prefers CP status.state, then the existing mirror value", () => {
    expect(resourceStatusText({ state: "running" }, "provisioning")).toBe("running");
    expect(resourceStatusText({}, "degraded")).toBe("degraded");
    expect(resourceStatusText(null, null)).toBe("provisioning");
    // Empty state string is "not populated yet", not a status.
    expect(resourceStatusText({ state: "" }, "running")).toBe("running");
  });

  // The CP/agent speak their own vocabulary; the mirror must store UI states so
  // aggregates that compare against them (running counts, project status chips)
  // don't silently miss (SIGMA-176/189).
  it("translates CP/agent state vocabulary into UI status", () => {
    expect(resourceStatusText({ state: "applied" }, null)).toBe("running");
    expect(resourceStatusText({ state: "failed" }, null)).toBe("error");
    // A dependent of a failed op — "did not deploy", not "unknown".
    expect(resourceStatusText({ state: "skipped" }, null)).toBe("error");
    expect(resourceStatusText({ state: "building" }, null)).toBe("provisioning");
    // An unrecognized state keeps the previous value rather than storing junk.
    expect(resourceStatusText({ state: "wat" }, "running")).toBe("running");
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
      kind: "mongodb",
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

  // SIGMA-161: with the CP's latest deployment supplied, version and
  // lastDeployAt track reality instead of freezing at resource creation.
  it("derives version + lastDeployAt from the latest CP deployment", () => {
    const row = resourceMirrorRow(cpResource(), null, {
      gitSha: "abcdef1234567890",
      status: "success",
      createdAt: "2026-08-01T10:00:00Z",
    });
    expect(row.version).toBe("abcdef12");
    expect(row.lastDeployAt).toEqual(new Date("2026-08-01T10:00:00Z"));
    // A failed latest deploy still moves lastDeployAt but keeps the prior version.
    const failed = resourceMirrorRow(
      cpResource(),
      { status: "running", repo: null, domain: null, version: "v7", lastDeployAt: new Date(0) },
      { gitSha: "abcdef1234567890", status: "failed", createdAt: "2026-08-02T10:00:00Z" }
    );
    expect(failed.version).toBe("v7");
    expect(failed.lastDeployAt).toEqual(new Date("2026-08-02T10:00:00Z"));
  });

  // SIGMA-194: preview resources carry their flag into the mirror.
  it("carries ephemeral", () => {
    expect(resourceMirrorRow(cpResource({ ephemeral: true })).ephemeral).toBe(true);
    expect(resourceMirrorRow(cpResource()).ephemeral).toBe(false);
  });
});

describe("deploy status mapping", () => {
  it("collapses in-flight CP states to running and passes terminals through", () => {
    expect(localDeployStatus("queued")).toBe("running");
    expect(localDeployStatus("building")).toBe("running");
    expect(localDeployStatus("deploying")).toBe("running");
    expect(localDeployStatus("success")).toBe("success");
    expect(localDeployStatus("failed")).toBe("failed");
    expect(localDeployStatus("superseded")).toBe("superseded");
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
      production: false,
      createdAt: new Date("2026-07-02T00:00:00Z"),
    });
  });

  // SIGMA-190: the flag that seeds backup retention survives the mirror now.
  it("carries production", () => {
    const env: CpEnvironment = {
      id: "env_p",
      orgId: "org_1",
      projectId: "prj_1",
      name: "live",
      production: true,
      createdAt: "2026-07-02T00:00:00Z",
    };
    expect(environmentMirrorRow(env).production).toBe(true);
  });
});
