/**
 * The Hugging Face models demo mode offers (SIGMA-213/214/215).
 *
 * Demo mode has no control plane, so nothing here can ask huggingface.co — and
 * the model step is the one screen where a prospective user learns that the
 * product sizes a model against their hardware BEFORE deploying it. A picker
 * that returned nothing offline would hide the whole feature from everyone
 * evaluating it.
 *
 * These are real repositories with their real parameter counts, and every card
 * is a RECORDING of what the control plane answers for it — the sizing figures
 * are written down as constants, not computed. That is the point: the VRAM
 * formula lives once, in cp/internal/hf/sizing.go, and a demo that re-derived it
 * in TypeScript would be a second implementation that drifts, which is the exact
 * defect the single-source design exists to prevent. Each figure below is
 *
 *   ceil(parameters × bytesPerParam × 1.20 ÷ 0.90)      → vramBytesRequired
 *   "~" + round(vramBytesRequired ÷ 1e9) + " GB"        → vramText
 *
 * evaluated once, by hand, at the time of writing.
 *
 * The set covers every branch the picker and the fit check can take:
 *
 *   Llama 3.1 8B Instruct    gated, fits a 40 GB card      → the ordinary path
 *   Llama 3.1 70B Instruct   gated, fits nothing in demo   → the VRAM refusal
 *   Mistral 7B Instruct      gated                         → the token gate
 *   Qwen2.5 7B Instruct      ungated, fits                 → walkable end to end
 *   TinyLlama 1.1B Chat      ungated, tiny                 → fits anything
 *   Llama 2 13B Chat AWQ     4-bit, sized from the NAME    → quantization
 *   phi-2 GGUF               unsizable, served by Ollama   → no fit check, and
 *                                                            a derived runtime
 *                                                            that is not vLLM
 */

import type { ModelCard } from "@/lib/wizard/llm";

export const MOCK_MODELS: ModelCard[] = [
  {
    id: "meta-llama/Llama-3.1-8B-Instruct",
    name: "Llama 3.1 8B Instruct",
    gated: true,
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
  },
  {
    id: "meta-llama/Llama-3.1-70B-Instruct",
    name: "Llama 3.1 70B Instruct",
    gated: true,
    downloads: 421_760,
    likes: 1_205,
    pipelineTag: "text-generation",
    library: "transformers",
    engine: "vllm",
    parameters: 70_553_706_496,
    parametersKnown: true,
    quantization: "none",
    bytesPerParam: 2,
    vramBytesRequired: 188_143_217_323,
    vramText: "~188 GB",
    sizingBasis: "safetensors",
  },
  {
    id: "mistralai/Mistral-7B-Instruct-v0.3",
    name: "Mistral 7B Instruct v0.3",
    gated: true,
    downloads: 1_204_776,
    likes: 1_682,
    pipelineTag: "text-generation",
    library: "transformers",
    engine: "vllm",
    parameters: 7_248_023_552,
    parametersKnown: true,
    quantization: "none",
    bytesPerParam: 2,
    vramBytesRequired: 19_328_062_806,
    vramText: "~19 GB",
    sizingBasis: "safetensors",
  },
  {
    id: "Qwen/Qwen2.5-7B-Instruct",
    name: "Qwen2.5 7B Instruct",
    gated: false,
    downloads: 986_431,
    likes: 1_104,
    pipelineTag: "text-generation",
    library: "transformers",
    engine: "vllm",
    parameters: 7_615_616_512,
    parametersKnown: true,
    quantization: "none",
    bytesPerParam: 2,
    vramBytesRequired: 20_308_310_699,
    vramText: "~20 GB",
    sizingBasis: "safetensors",
  },
  {
    id: "TinyLlama/TinyLlama-1.1B-Chat-v1.0",
    name: "TinyLlama 1.1B Chat v1.0",
    gated: false,
    downloads: 512_338,
    likes: 1_347,
    pipelineTag: "text-generation",
    library: "transformers",
    engine: "vllm",
    parameters: 1_100_048_384,
    parametersKnown: true,
    quantization: "none",
    bytesPerParam: 2,
    vramBytesRequired: 2_933_462_358,
    vramText: "~3 GB",
    sizingBasis: "safetensors",
  },
  {
    // Sized from the repo NAME rather than the safetensors index, and that is
    // correct rather than a fallback: an AWQ checkpoint packs eight 4-bit
    // weights into one int32 element, so the Hub's element count would size a
    // 13B model at well under a gigabyte (see hf.packsParameters).
    id: "TheBloke/Llama-2-13B-chat-AWQ",
    name: "Llama 2 13B Chat AWQ",
    gated: false,
    downloads: 88_204,
    likes: 132,
    pipelineTag: "text-generation",
    library: "transformers",
    engine: "vllm",
    parameters: 13_000_000_000,
    parametersKnown: true,
    quantization: "awq",
    bytesPerParam: 0.5,
    vramBytesRequired: 8_666_666_667,
    vramText: "~9 GB",
    sizingBasis: "name",
  },
  {
    // The unsizable one, and it is unsizable honestly: a GGUF-only repository
    // publishes no safetensors index, and "phi-2" carries no size token to
    // parse. It is also the demo's proof that the runtime is DERIVED — vLLM
    // cannot load GGUF weights, so this card says ollama and the step shows it
    // without asking.
    id: "TheBloke/phi-2-GGUF",
    name: "Phi-2 GGUF",
    gated: false,
    downloads: 143_090,
    likes: 268,
    pipelineTag: "text-generation",
    library: "transformers",
    engine: "ollama",
    parameters: 0,
    parametersKnown: false,
    quantization: "gguf",
    bytesPerParam: 0.6,
    vramBytesRequired: 0,
    vramText: "",
    sizingBasis: "unknown",
  },
];

/**
 * Whether demo mode claims a Hub token.
 *
 * FALSE, so the gated models above are refused at the model step exactly as
 * they would be on a control plane nobody has configured — which is the state
 * every self-hoster starts in, and the one where the refusal has to be
 * comprehensible. Pretending a token exists would make the demo's most-clicked
 * model deploy offline and fail for real users, which is the opposite of what
 * demo mode is for. Four ungated models remain, so the flow still walks to the
 * end.
 */
export const MOCK_TOKEN_CONFIGURED = false;

export function findMockModel(id: string): ModelCard | undefined {
  return MOCK_MODELS.find((m) => m.id === id.trim());
}

/** Matches the Hub's own behaviour closely enough to be useful: an empty query
 *  lists everything, which is what fills the picker before anyone types. */
export function searchMockModels(query: string, limit = 20): ModelCard[] {
  const q = query.trim().toLowerCase();
  const hits = q
    ? MOCK_MODELS.filter(
        (m) => m.id.toLowerCase().includes(q) || m.name.toLowerCase().includes(q)
      )
    : MOCK_MODELS;
  return hits.slice(0, limit);
}
