import { describe, expect, it } from "vitest";
import { MOCK_MODELS, MOCK_TOKEN_CONFIGURED, findMockModel, searchMockModels } from "./models";
import { servers } from "./data";
import { gatedWarning, modelStepError, serverFitsModel, unservableReason } from "@/lib/wizard/llm";
import { formatVram, requiredVramBytes } from "@/lib/server-catalog.generated";

/** The three cards the demo fleet reports (mock/data.ts), smallest first: an
 *  A10G at 22731 MiB, an A100 at 40960 MiB and an H100 at 81559 MiB. They are
 *  read out of the fixtures rather than restated, because a catalogue sized
 *  against a fleet it has drifted from is exactly the failure these tests are
 *  here to prevent. */
const CARDS = Object.fromEntries(
  servers
    .filter((sv) => (sv.facts?.gpu?.vramBytesPerGpu ?? 0) > 0)
    .map((sv) => [sv.name, { gpu: sv.facts!.gpu }] as const)
);
const SMALL = CARDS["gpu-a10g-01"];
const DEMO_GPU = CARDS["gpu-a100-01"];
const LARGE = CARDS["gpu-h100-01"];

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

  // The check only demonstrates anything when it can DISAGREE about two hosts.
  // A catalogue every card either takes or refuses in unison draws the same
  // verdict down a whole column, and an operator learns nothing about why the
  // product asked for their hardware in the first place.
  it("separates the demo's cards: a model each larger GPU takes and the one below it does not", () => {
    const refusedBySmall = MOCK_MODELS.filter(
      (m) => !serverFitsModel(m, SMALL).fits && serverFitsModel(m, DEMO_GPU).fits
    );
    const refusedByMiddle = MOCK_MODELS.filter(
      (m) => !serverFitsModel(m, DEMO_GPU).fits && serverFitsModel(m, LARGE).fits
    );
    expect(refusedBySmall.map((m) => m.id)).toEqual(["Qwen/Qwen2.5-14B-Instruct"]);
    expect(refusedByMiddle.map((m) => m.id)).toEqual([
      "hugging-quants/Meta-Llama-3.1-70B-Instruct-AWQ-INT4",
    ]);
  });

  // The refusal for an oversized model tells the operator to go and find "an
  // AWQ or GPTQ repository of the same model" (wizard/availability.ts). A demo
  // catalogue that held no such repository would send them looking for
  // something it does not have.
  it("carries the quantized build its own too-big refusal recommends", () => {
    const full = findMockModel("meta-llama/Llama-3.1-70B-Instruct")!;
    const quantized = findMockModel("hugging-quants/Meta-Llama-3.1-70B-Instruct-AWQ-INT4")!;
    expect(serverFitsModel(full, LARGE).fits).toBe(false);
    expect(serverFitsModel(quantized, LARGE).fits).toBe(true);
    expect(quantized.quantization).toBe("awq");
  });

  // Listed WITH the lock badge, warned about, and still pickable — the token
  // the warning names is the one the GPU host would pull with, and nothing here
  // can see whether the operator holds access to the repository.
  it("offers gated models, warned about for the real reason and not blocked", () => {
    const gated = MOCK_MODELS.filter((m) => m.gated);
    expect(gated.length).toBeGreaterThan(0);
    expect(gatedWarning(gated[0], MOCK_TOKEN_CONFIGURED)).toContain("HUGGING_FACE_HUB_TOKEN");
    expect(modelStepError(gated[0], gated[0].id)).toBeNull();
  });

  // A demo where every model is blocked is a demo of a wall.
  it("leaves models that deploy end to end offline", () => {
    const walkable = MOCK_MODELS.filter(
      (m) => !modelStepError(m, m.id) && serverFitsModel(m, DEMO_GPU).fits
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

  // A GGUF repository deploys as a container that starts, reports healthy and
  // serves nothing, so the model step refuses it — and a demo that never shows
  // that sentence hides the check from everyone evaluating the product.
  it("offers a GGUF repository, which the model step refuses", () => {
    const gguf = MOCK_MODELS.filter((m) => m.quantization === "gguf");
    expect(gguf.length).toBeGreaterThan(0);
    expect(unservableReason(gguf[0])).toContain("safetensors");
    expect(modelStepError(gguf[0], gguf[0].id)).toContain("GGUF");
  });

  // The refusal the SIZE check cannot make: whisper is ~4 GB, fits every card
  // in the demo fleet, and serves nothing a model endpoint offers. It is
  // reachable by pasting the id — which is exactly how it is reachable in
  // production, because no search returns it.
  it("carries a model whose task is wrong, and blocks the step on it", () => {
    const whisper = findMockModel("openai/whisper-large-v3");
    expect(whisper).toBeDefined();
    expect(serverFitsModel(whisper, DEMO_GPU).fits).toBe(true);
    expect(modelStepError(whisper!, whisper!.id)).toContain("text generation");
  });

  // The runtime is read from the card rather than asked for, and the control
  // plane renders exactly one for a picked model.
  it("records the runtime the control plane would render", () => {
    expect(new Set(MOCK_MODELS.map((m) => m.engine))).toEqual(new Set(["vllm"]));
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
      // A tenth of a gigabyte is the format hf.FormatVRAM emits below 100 GB,
      // and this guard used to demand a whole number — so it PASSED for the
      // stale fixtures and would have failed anyone who re-recorded them from a
      // live control plane, with a red suite telling them the truth was
      // malformed. It now accepts what the control plane actually sends.
      expect(model.vramText).toMatch(/^~\d+(\.\d)? (MB|GB)$/);
    } else {
      expect(model.parameters).toBe(0);
      expect(model.vramBytesRequired).toBe(0);
      expect(model.vramText).toBe("");
    }
  });

  // Both branches of the rendering the fixtures had drifted from: a tenth of a
  // gigabyte below 100 GB, because the GPU capacity this figure is set against
  // is TRUNCATED and "needs ~21 GB, has 21 GB" reads as a broken refusal; a
  // whole number rounded UP above it, where a tenth is noise.
  it("records each estimate the way the control plane renders it", () => {
    expect(findMockModel("meta-llama/Llama-3.1-8B-Instruct")?.vramText).toBe("~21.4 GB");
    expect(findMockModel("meta-llama/Llama-3.1-70B-Instruct")?.vramText).toBe("~189 GB");
  });

  // …and the figures themselves are the control plane's arithmetic, not numbers
  // that were once its arithmetic (SIGMA-279).
  //
  // These fixtures were evaluated by hand, once, from constants that live in
  // cp/internal/hf/sizing.go, and the assertions above compared them to
  // themselves. Changing UtilizationCap to 0.85 or KVActivationFactor to 1.30
  // left every suite green while demo mode went on telling evaluators that
  // Llama-3.1-8B needs ~21.4 GB against a product now saying ~22.7 GB — a
  // prospect sizing a GPU purchase from the demo buys the wrong card. Both
  // constants and the formatter's bands are now rendered into the generated
  // catalog, so the recomputation below is the control plane's own.
  it("vramBytesRequired equals the sizing formula from the generated constants", () => {
    for (const model of MOCK_MODELS) {
      if (!model.parametersKnown) continue;
      expect(model.vramBytesRequired, model.id).toBe(
        requiredVramBytes(model.parameters, model.bytesPerParam)
      );
      expect(model.vramText, model.id).toBe(formatVram(model.vramBytesRequired));
    }
  });
});

describe("the demo picker answers like the control plane's search", () => {
  it("lists every text-generation model before anything is typed", () => {
    expect(searchMockModels("").map((m) => m.id)).toEqual(
      MOCK_MODELS.filter((m) => m.pipelineTag === "text-generation").map((m) => m.id)
    );
  });

  // hf.Client.Search sends pipeline_tag=text-generation, so no control plane
  // can return a speech or embedding repository from a search. A demo that
  // listed one would be demonstrating behaviour the product does not have.
  it("never offers a model whose task the endpoint cannot serve", () => {
    expect(searchMockModels("whisper")).toEqual([]);
    expect(new Set(searchMockModels("").map((m) => m.pipelineTag))).toEqual(
      new Set(["text-generation"])
    );
  });

  it("matches on the repo id and on the display name", () => {
    expect(searchMockModels("qwen").map((m) => m.id)).toEqual([
      "Qwen/Qwen2.5-7B-Instruct",
      "Qwen/Qwen2.5-14B-Instruct",
    ]);
    // Both 70B repositories, in catalogue order — the full-precision one and
    // the quantized build an operator refused for size is sent to look for. A
    // search that offered only one of them would hide the answer.
    expect(searchMockModels("70B").map((m) => m.id)).toEqual([
      "meta-llama/Llama-3.1-70B-Instruct",
      "hugging-quants/Meta-Llama-3.1-70B-Instruct-AWQ-INT4",
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
