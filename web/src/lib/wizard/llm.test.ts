import { describe, expect, it } from "vitest";
import {
  downloadsText,
  gatedBlocked,
  looksLikeRepoId,
  modelStepError,
  parameterText,
  serverFitsModel,
  vramNeedText,
  type ModelCard,
} from "./llm";

/** A card as the control plane sends it. The sizing numbers are the CP's own —
 *  nothing in these tests recomputes them, which is the property under test as
 *  much as any assertion below. */
function card(over: Partial<ModelCard> = {}): ModelCard {
  return {
    id: "meta-llama/Llama-3.1-8B-Instruct",
    name: "Llama 3.1 8B Instruct",
    gated: false,
    downloads: 2_412_905,
    likes: 4_321,
    pipelineTag: "text-generation",
    library: "transformers",
    engine: "vllm",
    parameters: 8_030_261_248,
    parametersKnown: true,
    quantization: "none",
    bytesPerParam: 2,
    vramBytesRequired: 21_414_029_995,
    vramText: "~21 GB",
    sizingBasis: "safetensors",
    ...over,
  };
}

const GB = 1_000_000_000;

/** A host with one card of the given size. */
function gpu(vramBytesPerGpu: number, count = 1) {
  return { gpu: { vendor: "nvidia", model: "NVIDIA", count, vramBytesPerGpu } };
}

describe("a model that cannot fit the card is refused before the deploy", () => {
  it("accepts a model smaller than the GPU", () => {
    expect(serverFitsModel(card(), gpu(42 * GB)).fits).toBe(true);
  });

  it("refuses a model larger than the GPU and states both sizes", () => {
    const verdict = serverFitsModel(card(), gpu(16 * GB));
    expect(verdict.fits).toBe(false);
    // The model's figure is the control plane's rendered string, and the GPU's
    // is rendered the way the control plane's own refusal renders it — an
    // operator reading the wizard and the API error must see one pair of
    // numbers, not two.
    expect(verdict.reason).toBe(
      "This model needs about 21 GB of VRAM; this server's GPU has 16 GB."
    );
  });

  // vLLM loads the whole model into one card: the engine catalog renders no
  // --tensor-parallel-size, so two 16 GB cards run 16 GB models.
  it("compares against ONE card, never the sum of them", () => {
    expect(serverFitsModel(card(), gpu(16 * GB, 4)).fits).toBe(false);
  });

  it("fits a model exactly the size of the card", () => {
    expect(serverFitsModel(card({ vramBytesRequired: 24 * GB }), gpu(24 * GB)).fits).toBe(true);
  });
});

describe("an unknown never blocks a deploy", () => {
  // Refusing someone's own model on their own hardware over a number we guessed
  // is the one thing this feature must not do.
  it("runs no fit check at all when the parameter count is unknown", () => {
    const unsized = card({
      parametersKnown: false,
      parameters: 0,
      vramBytesRequired: 0,
      vramText: "",
      sizingBasis: "unknown",
    });
    expect(serverFitsModel(unsized, gpu(4 * GB)).fits).toBe(true);
    expect(serverFitsModel(unsized, gpu(4 * GB)).reason).toBeUndefined();
  });

  // An agent that predates SIGMA-201, or one whose nvidia probe failed this
  // tick, reports no inventory. Absent is UNKNOWN, never empty — the rule the
  // registration gate holds, and the difference between a filter and a fleet
  // that cannot deploy anything.
  it("runs no fit check against a server that reported no GPU facts", () => {
    expect(serverFitsModel(card(), {}).fits).toBe(true);
    expect(serverFitsModel(card(), { gpu: null }).fits).toBe(true);
    expect(serverFitsModel(card(), undefined).fits).toBe(true);
  });

  // "I looked and found a GPU" without a size is still a missing number, and a
  // missing number is not a zero-byte card.
  it("runs no fit check when the GPU reported no VRAM figure", () => {
    expect(serverFitsModel(card(), { gpu: { vendor: "nvidia", count: 1 } }).fits).toBe(true);
  });

  it("runs no fit check before a model is chosen", () => {
    expect(serverFitsModel(null, gpu(4 * GB)).fits).toBe(true);
  });
});

describe("a gated model with no Hub token is refused at the model step", () => {
  it("names the environment variable that fixes it", () => {
    const reason = gatedBlocked(card({ gated: true }), false);
    expect(reason).toContain("CP_HUGGING_FACE_TOKEN");
    expect(reason).toContain("meta-llama/Llama-3.1-8B-Instruct");
  });

  it("allows a gated model once the control plane holds a token", () => {
    expect(gatedBlocked(card({ gated: true }), true)).toBeNull();
  });

  it("allows an ungated model with no token", () => {
    expect(gatedBlocked(card({ gated: false }), false)).toBeNull();
  });
});

describe("the model step's continue gate", () => {
  it("asks for a model before anything is typed", () => {
    expect(modelStepError(null, "   ", true)).toContain("repo id");
  });

  // The control plane answers 404 for a Hub it could not reach, for a control
  // plane with no catalogue configured, and for an Ollama library tag — and
  // then creates the resource with the id exactly as typed. A wizard that
  // refused here would make the picker's dependency on huggingface.co a
  // dependency of deploying at all.
  it("lets an unresolved repo id through", () => {
    expect(modelStepError(null, "meta-llama/Llama-3.1-8B-Instruct", false)).toBeNull();
    expect(modelStepError(null, "llama3.2:3b", false)).toBeNull();
  });

  it("blocks a gated model this control plane cannot download", () => {
    const blocked = modelStepError(card({ gated: true }), "meta-llama/Llama-3.1-8B-Instruct", false);
    expect(blocked).toContain("CP_HUGGING_FACE_TOKEN");
  });

  it("lets a resolved, ungated model continue", () => {
    expect(modelStepError(card(), "meta-llama/Llama-3.1-8B-Instruct", false)).toBeNull();
  });
});

describe("a typed reference is offered to the Hub only when the Hub could answer", () => {
  it.each([
    ["meta-llama/Llama-3.1-8B-Instruct", true],
    ["TheBloke/phi-2-GGUF", true],
    ["llama3.2:3b", false],
    ["mistral", false],
    ["owner/repo/file.safetensors", false],
    ["   ", false],
  ])("%s → %s", (value, want) => {
    expect(looksLikeRepoId(value)).toBe(want);
  });
});

describe("the estimate is the control plane's own, not a second one", () => {
  // The tilde belongs to the picker's compact rendering; a sentence that says
  // "about" does not need it twice. The digits are never touched.
  it("quotes vramText, dropping only the tilde", () => {
    expect(vramNeedText(card())).toBe("21 GB");
    expect(vramNeedText(card({ vramText: "~700 MB" }))).toBe("700 MB");
  });

  // A control plane that sent bytes but no phrase still has to produce one, and
  // the same renderer the GPU's capacity uses keeps the two sides comparable.
  it("renders the byte count when the control plane sent no phrase", () => {
    expect(vramNeedText(card({ vramText: "" }))).toBe("21 GB");
  });
});

describe("the picker's one-line summaries", () => {
  it("reads parameter counts as the model's own headline number", () => {
    expect(parameterText(card())).toBe("8.0B params");
    expect(parameterText(card({ parameters: 1_100_048_384 }))).toBe("1.1B params");
    expect(parameterText(card({ parameters: 135_000_000 }))).toBe("135M params");
  });

  it("says so when there is no size rather than printing a zero", () => {
    expect(parameterText(card({ parametersKnown: false, parameters: 0 }))).toBe("size unknown");
  });

  it("shortens download counts", () => {
    expect(downloadsText(2_412_905)).toBe("2.4M downloads");
    expect(downloadsText(88_204)).toBe("88k downloads");
    expect(downloadsText(42)).toBe("42 downloads");
  });
});
