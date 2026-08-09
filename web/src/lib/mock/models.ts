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
 *   ceil(parameters × bytesPerParam × 1.20 ÷ 0.90)   → vramBytesRequired
 *
 * and vramText is hf.FormatVRAM of that, which renders
 *
 *   below 1000 MB      "~" + round(bytes ÷ 1e6) + " MB"
 *   below 100 GB       "~" + (bytes ÷ 1e9) to one decimal + " GB"
 *   at or above        "~" + ceil(bytes ÷ 1e9) + " GB"
 *
 * evaluated once, by hand, at the time of writing.
 *
 * THE TENTH OF A GIGABYTE IS NOT DECORATION, and this comment used to say the
 * figure was rounded to whole gigabytes — which is what these fixtures were
 * written from. The capacity a requirement is compared against is TRUNCATED by
 * the renderer on the other side of the sentence, so a whole-gigabyte estimate
 * produced refusals reading "needs ~17 GB; this server's GPU has 17 GB": a
 * refusal that reads as a bug in the refusal. Above 100 GB the estimate rounds
 * UP instead, which keeps the same guarantee without a decimal nobody reads at
 * that size. Re-record these from a live control plane and they must land on
 * exactly these strings.
 *
 * The set covers every branch the picker and the fit check can take, and it is
 * sized against the three cards the demo fleet reports (mock/data.ts): an A10G
 * at 23.8 GB, an A100 at 42.9 GB and an H100 at 85.5 GB. Two entries exist only
 * to make that ladder visible — a catalogue where every model either fits every
 * host or none of them demonstrates a check that never has to choose.
 *
 *   Llama 3.1 8B Instruct    gated, fits all three cards   → the ordinary path
 *   Llama 3.1 70B Instruct   gated, fits nothing in demo   → the VRAM refusal
 *   Mistral 7B Instruct      gated                         → the token gate
 *   Qwen2.5 7B Instruct      ungated, fits                 → walkable end to end
 *   Qwen2.5 14B Instruct     too big for the A10G ONLY     → the refusal that
 *                                                            names one host and
 *                                                            not its neighbour
 *   TinyLlama 1.1B Chat      ungated, tiny                 → fits anything
 *   Llama 2 13B Chat AWQ     4-bit, sized from the NAME    → quantization
 *   Llama 3.1 70B AWQ INT4   only the H100 takes it        → the quantized build
 *                                                            the too-big refusal
 *                                                            tells you to go find
 *   phi-2 GGUF               unsizable, and GGUF           → no fit check, and
 *                                                            the model step's
 *                                                            refusal
 *   whisper-large-v3         speech, not text generation   → the other refusal,
 *                                                            reachable by id
 *                                                            only (see
 *                                                            searchMockModels)
 */

import { TEXT_GENERATION_TASK, type ModelCard } from "@/lib/wizard/llm";

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
    vramText: "~21.4 GB",
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
    vramText: "~189 GB",
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
    vramText: "~19.3 GB",
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
    vramText: "~20.3 GB",
    sizingBasis: "safetensors",
  },
  {
    // The rung between the demo's cards: 39.4 GB is more than the A10G's 23.8
    // and less than the A100's 42.9, so one target step shows an eligible host
    // and a refused one side by side with the two numbers that decided it. Every
    // other sized model in this catalogue clears all three cards or none of
    // them, and a fit check that never has to choose between two hosts is a fit
    // check nobody can see working.
    id: "Qwen/Qwen2.5-14B-Instruct",
    name: "Qwen2.5 14B Instruct",
    gated: false,
    downloads: 341_882,
    likes: 618,
    pipelineTag: "text-generation",
    library: "transformers",
    engine: "vllm",
    parameters: 14_770_033_664,
    parametersKnown: true,
    quantization: "none",
    bytesPerParam: 2,
    vramBytesRequired: 39_386_756_438,
    vramText: "~39.4 GB",
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
    vramText: "~2.9 GB",
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
    vramText: "~8.7 GB",
    sizingBasis: "name",
  },
  {
    // The answer the too-big refusal gives, made reachable. Turned away from
    // every card for Llama 3.1 70B, the operator is told to find "an AWQ or GPTQ
    // repository of the same model" — and a demo catalogue that offered no such
    // repository would send them looking for something it does not have. This is
    // it: the same 70 billion parameters at half a byte each, 46.7 GB, which the
    // H100 takes and the A100 beside it does not.
    //
    // 70_000_000_000 exactly, because an AWQ checkpoint is sized from the repo
    // NAME — the same reason as the 13B entry above, and the reason the figure
    // is not the fp16 repository's 70_553_706_496.
    id: "hugging-quants/Meta-Llama-3.1-70B-Instruct-AWQ-INT4",
    name: "Llama 3.1 70B Instruct AWQ INT4",
    gated: false,
    downloads: 196_418,
    likes: 274,
    pipelineTag: "text-generation",
    library: "transformers",
    engine: "vllm",
    parameters: 70_000_000_000,
    parametersKnown: true,
    quantization: "awq",
    bytesPerParam: 0.5,
    vramBytesRequired: 46_666_666_667,
    vramText: "~46.7 GB",
    sizingBasis: "name",
  },
  {
    // The unsizable one, and it is unsizable honestly: a GGUF-only repository
    // publishes no safetensors index, and "phi-2" carries no size token to
    // parse. It is also the demo's GGUF pick, and therefore the one the model
    // step refuses: the engine is vllm because the control plane no longer
    // derives ollama from a GGUF repository (hf.EngineForModel), and vLLM cannot
    // load these weights. Recording "ollama" here would have the demo print
    // "Served by Ollama" directly above the sentence refusing the pick.
    id: "TheBloke/phi-2-GGUF",
    name: "Phi-2 GGUF",
    gated: false,
    downloads: 143_090,
    likes: 268,
    pipelineTag: "text-generation",
    library: "transformers",
    engine: "vllm",
    parameters: 0,
    parametersKnown: false,
    quantization: "gguf",
    bytesPerParam: 0.6,
    vramBytesRequired: 0,
    vramText: "",
    sizingBasis: "unknown",
  },
  {
    // The one the SIZE check cannot save anyone from: whisper sizes cleanly at
    // ~4.1 GB, fits every card in the demo fleet, and crash-loops a runtime that
    // serves text generation. Only the task refusal catches it, so the demo has
    // to be able to reach that sentence.
    //
    // It is deliberately NOT in the search list — see searchMockModels. The
    // control plane asks the Hub for text-generation repositories only, so no
    // search anywhere can return this card; a demo that listed it would be
    // showing a row the product cannot produce. It arrives the way it arrives in
    // production: someone pastes the id, and the picker resolves it.
    id: "openai/whisper-large-v3",
    name: "Whisper Large v3",
    gated: false,
    downloads: 3_412_884,
    likes: 4_012,
    pipelineTag: "automatic-speech-recognition",
    library: "transformers",
    engine: "vllm",
    parameters: 1_543_304_960,
    parametersKnown: true,
    quantization: "none",
    bytesPerParam: 2,
    vramBytesRequired: 4_115_479_894,
    vramText: "~4.1 GB",
    sizingBasis: "safetensors",
  },
];

/**
 * Whether demo mode claims a token to DOWNLOAD weights with.
 *
 * FALSE, so the gated models above carry the warning they would carry on a
 * control plane nobody has configured — the state every self-hoster starts in,
 * and the one where the sentence has to be comprehensible. Pretending a token
 * exists would let the demo's most-clicked model look ready to deploy while a
 * real pull 401s, which is the opposite of what demo mode is for. The warning is
 * not a wall: a gated pick still continues, here as everywhere, because nothing
 * in this process can see whether the operator has been granted access.
 */
export const MOCK_TOKEN_CONFIGURED = false;

/** One repo id, exactly as typed — the demo's half of the resolve route, which
 *  is NOT task-filtered on the control plane either. This is how a model the
 *  search will never offer still reaches the picker, and therefore how the
 *  refusal for it is reachable offline. */
export function findMockModel(id: string): ModelCard | undefined {
  return MOCK_MODELS.find((m) => m.id === id.trim());
}

/**
 * The demo's search: substring over the catalogue, an empty query lists
 * everything, which is what fills the picker before anyone types.
 *
 * Text generation only, because that is the query the control plane sends —
 * hf.Client.Search sets pipeline_tag=text-generation, so no search on any
 * control plane can return a speech or embedding repository. Listing one here
 * would be a demo of behaviour the product does not have, and the row it
 * offered would be refused the moment it was clicked.
 */
export function searchMockModels(query: string, limit = 20): ModelCard[] {
  const q = query.trim().toLowerCase();
  const listed = MOCK_MODELS.filter((m) => m.pipelineTag === TEXT_GENERATION_TASK);
  const hits = q
    ? listed.filter((m) => m.id.toLowerCase().includes(q) || m.name.toLowerCase().includes(q))
    : listed;
  return hits.slice(0, limit);
}
