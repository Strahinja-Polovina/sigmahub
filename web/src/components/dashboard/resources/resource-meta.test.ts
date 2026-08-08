import { describe, expect, it } from "vitest";
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
    for (const kind of ["app", "postgres", "mysql", "mongodb", "redis", "s3", "llm"]) {
      expect(KIND_LABELS[kind as keyof typeof KIND_LABELS]).toBeTruthy();
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
