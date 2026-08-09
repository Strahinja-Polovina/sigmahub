import { describe, expect, it } from "vitest";
import {
  DEFAULT_LLM_ENGINE,
  DEFAULT_S3_ENGINE,
  LLM_ENGINES,
  S3_ENGINES,
  defaultManagedName,
  isDatabaseKind,
  isManagedKind,
  managedSummary,
  resourceNameError,
} from "./managed";
import {
  DEFAULT_S3_ENGINE as CATALOG_DEFAULT_S3_ENGINE,
  S3_ENGINE_NAMES,
} from "@/lib/server-catalog.generated";

describe("which kinds skip the application flow", () => {
  it("classes every engine as managed and the app as not", () => {
    for (const kind of ["postgres", "mysql", "mongodb", "redis", "s3"] as const) {
      expect(isManagedKind(kind), kind).toBe(true);
    }
    expect(isManagedKind("app")).toBe(false);
    // An inference endpoint has its own two decisions (runtime and model), so
    // it is not managed-data even though it has no repository.
    expect(isManagedKind("llm")).toBe(false);
    expect(isDatabaseKind("s3")).toBe(false);
    expect(isDatabaseKind("redis")).toBe(true);
  });
});

describe("engine catalogs are enumerations, not free text", () => {
  // The control plane refuses an unknown engine at create, so an input box
  // here would be a field whose only outcomes are "a value from this list" and
  // "a 422 after the wizard closed".
  // Asserted against the GENERATED catalog, not against a literal typed here.
  // The previous version of this test compared the list to a copy of itself and
  // to `S3_ENGINES[0].id`, so it stayed green through every disagreement that
  // mattered: a third engine added to the Go catalog was provisioned by the
  // control plane and never offered by the wizard, and renaming one left the
  // picker sending a value create rejects with a 422 after the dialog closed.
  it("offers exactly the object-storage engines the control plane provisions", () => {
    expect(S3_ENGINES.map((e) => e.id)).toEqual(S3_ENGINE_NAMES);
  });

  it("defaults to the control plane's default, not to whichever is listed first", () => {
    expect(DEFAULT_S3_ENGINE).toBe(CATALOG_DEFAULT_S3_ENGINE);
  });

  it("gives every offered engine a sentence to choose it by", () => {
    for (const engine of S3_ENGINES) {
      expect(engine.label, `${engine.id} has no label`).toBeTruthy();
      expect(engine.detail, `${engine.id} has no detail`).toBeTruthy();
    }
  });

  // The inference runtimes are still a hand-kept list. They are not generated
  // because nothing about them reaches the web today except two names — but
  // they are the same shape as the S3 list above was, and the same failure is
  // available to them the moment the control plane gains a third runtime.
  it("names the inference runtimes the control plane knows how to render", () => {
    expect(LLM_ENGINES.map((e) => e.id)).toEqual(["vllm", "ollama"]);
    expect(DEFAULT_LLM_ENGINE).toBe(LLM_ENGINES[0].id);
  });
});

describe("what the operator is told before committing", () => {
  it("says where the credentials will be, for both managed shapes", () => {
    expect(managedSummary("postgres").credentials).toContain("Database panel");
    expect(managedSummary("s3").credentials).toContain("Storage panel");
  });

  // The CP pins exactly one image per engine and the dashboard cannot read that
  // pin; printing a number here would create a fifth place the version is
  // written down and a first place it can be wrong.
  it("describes the pin instead of inventing a version number", () => {
    expect(managedSummary("postgres").line).toContain("pinned by this control plane");
    expect(managedSummary("postgres").line).not.toMatch(/\d+\.\d+/);
  });
});

describe("names", () => {
  // `redis` alone collides the moment a second one is created in the same
  // environment, and the failure arrives from the CP after the wizard closed.
  it("suffixes a managed default with the environment", () => {
    expect(defaultManagedName("redis", "production")).toBe("redis-production");
    expect(defaultManagedName("s3", "staging")).toBe("storage-staging");
    expect(defaultManagedName("redis")).toBe("redis");
  });

  it("sanitizes an environment name into the label", () => {
    expect(defaultManagedName("redis", "PR Preview #4")).toBe("redis-pr-preview-4");
  });

  it("refuses names that cannot be a container, DNS label or volume", () => {
    expect(resourceNameError("")).toContain("required");
    expect(resourceNameError("Shop")).toContain("lowercase");
    expect(resourceNameError("-shop")).toContain("lowercase");
    expect(resourceNameError("shop-")).toContain("lowercase");
    expect(resourceNameError("sh op")).toContain("lowercase");
    expect(resourceNameError("x".repeat(41))).toContain("lowercase");
    expect(resourceNameError("shop-api-2")).toBeNull();
  });
});
