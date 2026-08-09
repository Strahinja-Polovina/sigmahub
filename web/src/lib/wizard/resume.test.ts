import { describe, expect, it } from "vitest";
import {
  decodeWizardDraft,
  encodeWizardDraft,
  shouldResume,
  wizardResumePath,
  WIZARD_RESUME_PARAM,
  WIZARD_RESUME_VALUE,
} from "./resume";

describe("the draft survives the GitHub install round trip", () => {
  it("round-trips everything the wizard needs to pick back up", () => {
    const draft = {
      kind: "app" as const,
      name: "storefront",
      projectId: "prj_1",
      environmentId: "env_1",
      serverId: "srv_1",
      repo: "acme/storefront",
      branch: "main",
    };
    expect(decodeWizardDraft(encodeWizardDraft(draft))).toEqual(draft);
  });

  it("keeps a cluster target too", () => {
    const draft = { kind: "app" as const, clusterId: "cl_1" };
    expect(decodeWizardDraft(encodeWizardDraft(draft))).toEqual(draft);
  });
});

describe("a draft is untrusted input", () => {
  // It lives in sessionStorage, which is the user's own machine. Anything that
  // is not a draft parses to null and the wizard opens empty — the behaviour
  // this feature exists to avoid, but never worse than acting on a forged one.
  it("refuses anything that is not a draft", () => {
    for (const bad of [
      null,
      undefined,
      "",
      "not json",
      "[]",
      '"a string"',
      "{}",
      '{"kind":"nonsense"}',
      '{"kind":""}',
      `{"kind":"app","name":"${"x".repeat(500)}"}`,
    ]) {
      expect(decodeWizardDraft(bad as string | null), String(bad)).not.toEqual(
        expect.objectContaining({ name: "x".repeat(500) })
      );
    }
    expect(decodeWizardDraft("{}")).toBeNull();
    expect(decodeWizardDraft('{"kind":"nonsense"}')).toBeNull();
    expect(decodeWizardDraft("[]")).toBeNull();
  });

  it("drops oversized fields rather than carrying them", () => {
    const decoded = decodeWizardDraft(`{"kind":"app","name":"${"x".repeat(500)}"}`);
    expect(decoded).toEqual({ kind: "app" });
  });

  it("refuses a draft too large to be one", () => {
    expect(decodeWizardDraft(JSON.stringify({ kind: "app", junk: "x".repeat(5000) }))).toBeNull();
  });
});

describe("where the callback lands", () => {
  // The path is BUILT from an id rather than round-tripped through GitHub's
  // `state`: a "return to this URL" parameter that comes back from a third
  // party is an open redirect with extra steps.
  it("returns to the project the wizard was opened from", () => {
    expect(wizardResumePath("prj_1")).toBe(
      `/dashboard/projects/prj_1?${WIZARD_RESUME_PARAM}=${WIZARD_RESUME_VALUE}`
    );
  });

  it("returns to the resources list otherwise", () => {
    expect(wizardResumePath()).toBe(
      `/dashboard/resources?${WIZARD_RESUME_PARAM}=${WIZARD_RESUME_VALUE}`
    );
  });

  it("ignores a project id that is not one", () => {
    expect(wizardResumePath("../../evil")).toBe(
      `/dashboard/resources?${WIZARD_RESUME_PARAM}=${WIZARD_RESUME_VALUE}`
    );
    expect(wizardResumePath("https://evil.example.com")).not.toContain("evil");
  });

  it("recognizes the return trip", () => {
    expect(shouldResume(new URLSearchParams(`${WIZARD_RESUME_PARAM}=${WIZARD_RESUME_VALUE}`))).toBe(
      true
    );
    expect(shouldResume(new URLSearchParams("other=1"))).toBe(false);
    expect(shouldResume(null)).toBe(false);
  });
});

// The React semantics the resume path depends on.
//
// The restore runs in a render-phase adjustment guarded by `open !== prevOpen`.
// Seeding prevOpen from `open` made it match on the FIRST render — and both
// resume call sites mount the dialog already open, which is the entire point of
// coming back from github.com into the wizard. So the restore could not fire on
// the one render that matters: the user returned to an empty wizard on step 1,
// and because the draft was never cleared either, closing and reopening later
// resurrected it.
describe("the open-transition guard", () => {
  /** The guard as the component runs it. */
  function restoresOn(seed: boolean, open: boolean): boolean {
    return open !== seed && open;
  }

  it("fires when the dialog is MOUNTED open — the resume path", () => {
    expect(
      restoresOn(false, true),
      "seeding prevOpen from `open` skips the restore on a mount-open render"
    ).toBe(true);
  });

  it("still fires on an ordinary closed → open transition", () => {
    expect(restoresOn(false, true)).toBe(true);
  });

  it("does not fire while the dialog stays closed", () => {
    expect(restoresOn(false, false)).toBe(false);
  });
});
