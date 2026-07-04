import { describe, it, expect } from "vitest";
import {
  SERVER_TYPE_LABELS,
  RESOURCE_KIND_LABELS,
  SERVER_TYPE_ORDER,
  formatDate,
} from "./server-meta";

describe("SERVER_TYPE_LABELS", () => {
  it("has a label for every server type", () => {
    const expectedTypes = ["general", "storage", "database", "gpu"];
    for (const t of expectedTypes) {
      expect(SERVER_TYPE_LABELS[t as keyof typeof SERVER_TYPE_LABELS]).toBeDefined();
    }
  });

  it("maps general to General", () => {
    expect(SERVER_TYPE_LABELS.general).toBe("General");
  });

  it("maps gpu to GPU", () => {
    expect(SERVER_TYPE_LABELS.gpu).toBe("GPU");
  });
});

describe("RESOURCE_KIND_LABELS", () => {
  it("has a label for every resource kind", () => {
    const expectedKinds = ["app", "postgres", "mysql", "mongo", "redis", "s3", "llm"];
    for (const kind of expectedKinds) {
      expect(
        RESOURCE_KIND_LABELS[kind as keyof typeof RESOURCE_KIND_LABELS],
      ).toBeDefined();
    }
  });

  it("maps postgres to PostgreSQL", () => {
    expect(RESOURCE_KIND_LABELS.postgres).toBe("PostgreSQL");
  });

  it("maps llm to LLM", () => {
    expect(RESOURCE_KIND_LABELS.llm).toBe("LLM");
  });
});

describe("SERVER_TYPE_ORDER", () => {
  it("contains all four server types", () => {
    expect(SERVER_TYPE_ORDER).toHaveLength(4);
    expect(SERVER_TYPE_ORDER).toContain("general");
    expect(SERVER_TYPE_ORDER).toContain("database");
    expect(SERVER_TYPE_ORDER).toContain("storage");
    expect(SERVER_TYPE_ORDER).toContain("gpu");
  });

  it("general comes first", () => {
    expect(SERVER_TYPE_ORDER[0]).toBe("general");
  });
});

describe("formatDate", () => {
  it("formats an ISO date string", () => {
    const result = formatDate("2027-01-12");
    expect(result).toContain("Jan");
    expect(result).toContain("2027");
  });

  it("accepts a Date object", () => {
    const result = formatDate(new Date("2027-06-15"));
    expect(result).toContain("Jun");
    expect(result).toContain("2027");
  });
});
