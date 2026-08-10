import { describe, expect, it } from "vitest";
import {
  DEFAULT_LLM_ENGINE,
  DEFAULT_S3_ENGINE,
  LLM_ENGINES,
  S3_ENGINES,
  defaultManagedName,
  isDatabaseKind,
  isManagedKind,
  llmEngineLabel,
  managedSummary,
  resourceNameError,
} from "./managed";
import {
  DB_ENGINE_KINDS,
  DEFAULT_LLM_ENGINE as CATALOG_DEFAULT_LLM_ENGINE,
  DEFAULT_S3_ENGINE as CATALOG_DEFAULT_S3_ENGINE,
  LLM_ENGINE_NAMES,
  S3_ENGINE_NAMES,
  type ResourceKind,
} from "@/lib/server-catalog.generated";

describe("which kinds skip the application flow", () => {
  it("classes every engine as managed and the app as not", () => {
    // Every engine the control plane provisions, plus the bucket — asked of the
    // catalog, because the point of the assertion is that a NEW engine is
    // managed too, and a list typed out here could only ever cover the ones
    // somebody remembered (SIGMA-216).
    const managed: ResourceKind[] = [...DB_ENGINE_KINDS, "s3"];
    for (const kind of managed) {
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

  // The inference runtimes were the last hand-kept copy in this file, and they
  // failed the same way the S3 list did (SIGMA-278): renaming or replacing the
  // default runtime in store.llmEngines left the wizard sending engine "vllm"
  // for every model whose card did not resolve — a Hub timeout, a CP with no
  // Hub catalogue, a pasted repo id the Hub does not know — and provisionLLMTx
  // answered `unknown inference runtime "vllm"` as a 422 at the end of the LLM
  // wizard, with every Go and TypeScript suite green.
  it("LLM_ENGINES matches the generated catalog", () => {
    expect(LLM_ENGINES.map((e) => e.id)).toEqual(LLM_ENGINE_NAMES);
    expect(DEFAULT_LLM_ENGINE).toBe(CATALOG_DEFAULT_LLM_ENGINE);
  });

  it("gives every offered runtime a sentence to choose it by", () => {
    for (const engine of LLM_ENGINES) {
      expect(engine.label, `${engine.id} has no label`).toBeTruthy();
      expect(engine.detail, `${engine.id} has no detail`).toBeTruthy();
    }
  });

  it("prints an unknown runtime as itself rather than as undefined", () => {
    // The control plane's list can outrun this build's catalog.
    expect(llmEngineLabel("some-future-runtime")).toBe("some-future-runtime");
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
