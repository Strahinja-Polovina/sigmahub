import { describe, expect, it } from "vitest";
import { MOCK_MODELS, MOCK_TOKEN_CONFIGURED, findMockModel, searchMockModels } from "./models";
import { gatedBlocked, serverFitsModel } from "@/lib/wizard/llm";

/** The GPU the demo fleet ships with (mock/data.ts, gpu-a100-01): one 40 GiB
 *  A100, which is what makes the fit check mean something offline. */
const DEMO_GPU = { gpu: { vendor: "nvidia", count: 1, vramBytesPerGpu: 42_949_672_960 } };

describe("the demo catalogue walks every branch of the model step", () => {
  it("offers a model that fits the demo GPU and one that cannot", () => {
    const fits = MOCK_MODELS.filter((m) => serverFitsModel(m, DEMO_GPU).fits);
    const refused = MOCK_MODELS.filter((m) => !serverFitsModel(m, DEMO_GPU).fits);
    expect(fits.length).toBeGreaterThan(0);
    expect(refused.length).toBeGreaterThan(0);
  });

  // Without one, nobody evaluating the product ever sees the sentence that is
  // the whole point of SIGMA-214.
  it("states both sizes when a model is too big for the demo GPU", () => {
    const big = MOCK_MODELS.find((m) => !serverFitsModel(m, DEMO_GPU).fits);
    expect(serverFitsModel(big, DEMO_GPU).reason).toContain("42 GB");
  });

  it("offers a gated model, which demo mode refuses for the real reason", () => {
    const gated = MOCK_MODELS.filter((m) => m.gated);
    expect(gated.length).toBeGreaterThan(0);
    expect(gatedBlocked(gated[0], MOCK_TOKEN_CONFIGURED)).toContain("CP_HUGGING_FACE_TOKEN");
  });

  // A demo where every model is blocked is a demo of a wall.
  it("leaves ungated models that deploy end to end offline", () => {
    const walkable = MOCK_MODELS.filter(
      (m) => !gatedBlocked(m, MOCK_TOKEN_CONFIGURED) && serverFitsModel(m, DEMO_GPU).fits
    );
    expect(walkable.length).toBeGreaterThan(0);
  });

  it("offers a model nothing can size, so the no-fit-check path is walkable", () => {
    const unsized = MOCK_MODELS.filter((m) => !m.parametersKnown);
    expect(unsized.length).toBeGreaterThan(0);
    expect(serverFitsModel(unsized[0], { gpu: { vendor: "nvidia", vramBytesPerGpu: 1 } }).fits).toBe(
      true
    );
  });

  // The runtime is derived from the model, so the demo needs a model that
  // derives something other than the default.
  it("offers a model served by each runtime", () => {
    expect(new Set(MOCK_MODELS.map((m) => m.engine))).toEqual(new Set(["vllm", "ollama"]));
  });

  it("covers both sizing bases plus the absence of one", () => {
    expect(new Set(MOCK_MODELS.map((m) => m.sizingBasis))).toEqual(
      new Set(["safetensors", "name", "unknown"])
    );
  });
});

describe("every recorded card is internally consistent", () => {
  // A fixture that claimed a size while reporting parametersKnown false would
  // switch a fit check back ON with a number nobody derived — the failure the
  // flag exists to prevent, planted in the demo.
  it.each(MOCK_MODELS.map((m) => [m.id, m] as const))("%s", (_id, model) => {
    if (model.parametersKnown) {
      expect(model.parameters).toBeGreaterThan(0);
      expect(model.vramBytesRequired).toBeGreaterThan(0);
      expect(model.vramText).toMatch(/^~\d+ (MB|GB)$/);
    } else {
      expect(model.parameters).toBe(0);
      expect(model.vramBytesRequired).toBe(0);
      expect(model.vramText).toBe("");
    }
  });
});

describe("the demo picker answers like the control plane's search", () => {
  it("lists everything before anything is typed", () => {
    expect(searchMockModels("")).toHaveLength(MOCK_MODELS.length);
  });

  it("matches on the repo id and on the display name", () => {
    expect(searchMockModels("qwen").map((m) => m.id)).toEqual(["Qwen/Qwen2.5-7B-Instruct"]);
    expect(searchMockModels("70B").map((m) => m.id)).toEqual([
      "meta-llama/Llama-3.1-70B-Instruct",
    ]);
  });

  it("honours the limit the picker asks for", () => {
    expect(searchMockModels("", 2)).toHaveLength(2);
  });

  it("resolves a pasted id exactly, so the fit check applies to it too", () => {
    expect(findMockModel("  meta-llama/Llama-3.1-8B-Instruct  ")?.name).toBe(
      "Llama 3.1 8B Instruct"
    );
    expect(findMockModel("meta-llama/llama-3.1-8b-instruct")).toBeUndefined();
  });
});
