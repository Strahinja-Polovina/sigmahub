import { describe, it, expect } from "vitest";
import {
  KIND_LABELS,
  SERVER_TYPE_LABELS,
  DEPLOY_STATUS_META,
  formatDate,
  formatDateTime,
  formatDuration,
} from "./resource-meta";

describe("KIND_LABELS", () => {
  it("has a label for every resource kind", () => {
    const expectedKinds = ["app", "postgres", "mysql", "mongo", "redis", "s3", "llm"];
    for (const kind of expectedKinds) {
      expect(KIND_LABELS[kind as keyof typeof KIND_LABELS]).toBeDefined();
    }
  });

  it("maps app to App", () => {
    expect(KIND_LABELS.app).toBe("App");
  });

  it("maps postgres to PostgreSQL", () => {
    expect(KIND_LABELS.postgres).toBe("PostgreSQL");
  });

  it("maps s3 to Object storage", () => {
    expect(KIND_LABELS.s3).toBe("Object storage");
  });
});

describe("SERVER_TYPE_LABELS", () => {
  it("has a label for every server type", () => {
    const expectedTypes = ["general", "database", "storage", "gpu"];
    for (const t of expectedTypes) {
      expect(SERVER_TYPE_LABELS[t as keyof typeof SERVER_TYPE_LABELS]).toBeDefined();
    }
  });

  it("maps gpu to GPU", () => {
    expect(SERVER_TYPE_LABELS.gpu).toBe("GPU");
  });
});

describe("DEPLOY_STATUS_META", () => {
  it("has entries for queued, running, success, failed, building", () => {
    for (const status of ["queued", "running", "success", "failed", "building"]) {
      const meta = DEPLOY_STATUS_META[status];
      expect(meta).toBeDefined();
      expect(meta.label).toBeTruthy();
      expect(meta.text).toBeTruthy();
      expect(meta.dot).toBeTruthy();
    }
  });

  it("success has emerald styling", () => {
    expect(DEPLOY_STATUS_META.success.text).toContain("emerald");
    expect(DEPLOY_STATUS_META.success.dot).toContain("emerald");
  });

  it("failed has red styling", () => {
    expect(DEPLOY_STATUS_META.failed.text).toContain("red");
    expect(DEPLOY_STATUS_META.failed.dot).toContain("red");
  });
});

describe("formatDate", () => {
  it("formats an ISO date string", () => {
    const result = formatDate("2027-03-02T09:12:00Z");
    expect(result).toContain("2027");
    expect(result).toContain("Mar");
  });

  it("accepts a Date object", () => {
    const result = formatDate(new Date("2027-01-15"));
    expect(result).toContain("Jan");
    expect(result).toContain("2027");
  });
});

describe("formatDateTime", () => {
  it("formats an ISO datetime string with time component", () => {
    const result = formatDateTime("2027-03-02T09:12:00Z");
    expect(result).toContain("Mar");
  });

  it("accepts a Date object", () => {
    const result = formatDateTime(new Date("2027-06-15T14:30:00Z"));
    expect(result).toContain("Jun");
  });
});

describe("formatDuration", () => {
  it("formats seconds under 60 as Xs", () => {
    expect(formatDuration(45)).toBe("45s");
  });

  it("formats exactly 60 seconds as 1m 00s", () => {
    expect(formatDuration(60)).toBe("1m 00s");
  });

  it("formats 90 seconds as 1m 30s", () => {
    expect(formatDuration(90)).toBe("1m 30s");
  });

  it("pads remaining seconds to 2 digits", () => {
    expect(formatDuration(65)).toBe("1m 05s");
  });

  it("formats 0 seconds as 0s", () => {
    expect(formatDuration(0)).toBe("0s");
  });

  it("formats large values correctly", () => {
    expect(formatDuration(3661)).toBe("61m 01s");
  });
});
