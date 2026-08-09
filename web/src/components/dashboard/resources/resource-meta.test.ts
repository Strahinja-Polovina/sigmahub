import { describe, expect, it } from "vitest";
import { RESOURCE_KINDS } from "@/lib/server-catalog.generated";
import { formatDuration, KIND_LABELS, DEPLOY_STATUS_META } from "./resource-meta";

describe("formatDuration", () => {
  it("renders seconds under a minute", () => {
    expect(formatDuration(0)).toBe("0s");
    expect(formatDuration(59)).toBe("59s");
  });
  it("renders minutes with zero-padded seconds", () => {
    expect(formatDuration(60)).toBe("1m 00s");
    expect(formatDuration(61)).toBe("1m 01s");
    expect(formatDuration(754)).toBe("12m 34s");
  });
});

describe("resource kind labels", () => {
  it("covers every kind the availability matrix knows", () => {
    // Read from the catalog rather than typed out: this list WAS the seven
    // kinds of the day, so the test claimed to cover every kind while covering
    // whichever ones someone last remembered — the eighth would have been
    // labelled "undefined" on the resource header with this assertion green.
    for (const kind of RESOURCE_KINDS) {
      expect(KIND_LABELS[kind], kind).toBeTruthy();
    }
  });
});

describe("deploy status meta", () => {
  it("covers the CP pipeline states", () => {
    for (const status of ["queued", "running", "success", "failed"]) {
      expect(DEPLOY_STATUS_META[status]).toBeTruthy();
    }
  });
});
