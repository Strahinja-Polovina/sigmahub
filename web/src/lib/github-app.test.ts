import { describe, expect, it } from "vitest";
import {
  encodeInstallState,
  githubInstallUrl,
  isInstallationId,
  parseInstallState,
} from "./github-app";

describe("install state round-trip", () => {
  it("encodes and parses a project target", () => {
    const state = encodeInstallState({ kind: "project", projectId: "prj_1" });
    expect(state).toBe("proj:prj_1");
    expect(parseInstallState(state)).toEqual({ kind: "project", projectId: "prj_1" });
  });

  it("encodes and parses a connection target", () => {
    const state = encodeInstallState({
      kind: "connection",
      projectId: "prj_1",
      connectionId: "gcn_2",
    });
    expect(parseInstallState(state)).toEqual({
      kind: "connection",
      projectId: "prj_1",
      connectionId: "gcn_2",
    });
  });

  it("rejects forged or garbled state instead of guessing", () => {
    for (const bad of [
      undefined,
      null,
      "",
      "proj:",
      "proj:../etc",
      "proj:a:b",
      "conn:prj_1",
      "conn:prj_1:g c n",
      "nope:prj_1",
      "proj:" + "x".repeat(65),
    ]) {
      expect(parseInstallState(bad as string | null | undefined)).toBeNull();
    }
  });
});

describe("installation id validation", () => {
  it("accepts numeric GitHub ids only", () => {
    expect(isInstallationId("42")).toBe(true);
    expect(isInstallationId("")).toBe(false);
    expect(isInstallationId("42abc")).toBe(false);
    expect(isInstallationId(undefined)).toBe(false);
  });
});

describe("install url", () => {
  it("targets the app slug and carries the state", () => {
    expect(
      githubInstallUrl("sigmahub", { kind: "project", projectId: "prj_1" })
    ).toBe("https://github.com/apps/sigmahub/installations/new?state=proj%3Aprj_1");
  });
});
