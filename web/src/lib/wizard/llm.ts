/**
 * The model step's verdicts: can anything here serve this model, does it fit
 * that GPU, and is its download going to be authorised (SIGMA-213, SIGMA-214).
 *
 * Only the first two are refusals. The third is a warning, because it is the one
 * of the three we cannot check — see gatedWarning.
 *
 * The LLM path used to ask for two things it had no way to check. "Runtime" was
 * a dropdown with exactly one right answer that the operator had to look up —
 * vLLM cannot load GGUF weights and Ollama does not want a safetensors
 * repository, so every wrong pick was a container that would not start. "Model"
 * was a free-text field, so a typo, a gated repository, or a 70B model aimed at
 * a 24 GB card all became the same thing: a resource that is created, billed at
 * GPU rates, pulls tens of gigabytes and then dies in `docker logs` with a
 * message that names no model and suggests no fix.
 *
 * All three of those are knowable BEFORE the deploy, and the control plane knows
 * them. This module is the dashboard's half of that: it compares numbers it was
 * handed and turns them into the sentence the operator reads. It derives none of
 * them.
 *
 * THE ARITHMETIC IS NOT HERE, DELIBERATELY. The VRAM formula lives once, in
 * cp/internal/hf/sizing.go, and arrives on the card as both a byte count
 * (vramBytesRequired) and the rendered phrase (vramText). This repository's
 * recurring defect is two sides of one question computing it separately and then
 * disagreeing — the compatibility gate (SIGMA-203) is kept in step with the Go
 * one by a shared fixture for exactly this reason, and the create-time fit check
 * in cp/internal/store/llm_fit.go quotes vramText character for character so the
 * refusal and the picker cannot say different things. A `parameters * 2 * 1.2`
 * anywhere in this file would recreate the bug the design removes.
 */

import { formatReportedBytes, type HostFacts } from "@/lib/server-compat";

/**
 * One Hugging Face repository, as GET /v1/orgs/{orgId}/llm/models describes it.
 *
 * Mirrors hf.ModelCard field for field, including the ones the picker only
 * displays: the wire shape is declared once so the CP client, the demo
 * catalogue and the UI cannot drift into three slightly different ideas of what
 * a model is.
 */
export type ModelCard = {
  /** The repo id, e.g. "meta-llama/Llama-3.1-8B-Instruct". Case-sensitive. */
  id: string;
  name: string;
  /** Whether the Hub serves the weights only to a granted account. */
  gated: boolean;
  downloads: number;
  likes: number;
  pipelineTag: string;
  library: string;
  /** The runtime the control plane WOULD render for this model. Read, never
   *  asked: see hf.EngineForModel. */
  engine: string;
  /** 0 when unknown — always paired with parametersKnown false. */
  parameters: number;
  /** False when neither the safetensors index nor the repo id yielded a size.
   *  This single flag is what switches the fit check off rather than guessing a
   *  number to gate a deploy on. */
  parametersKnown: boolean;
  /** none | awq | gptq | fp8 | gguf */
  quantization: string;
  bytesPerParam: number;
  /** The estimate, in bytes. 0 when parametersKnown is false. */
  vramBytesRequired: number;
  /** The same figure rendered CP-side ("~21 GB"), empty when unsized. */
  vramText: string;
  /** safetensors | name | unknown */
  sizingBasis: string;
};

/** What a search answered, plus the one fact the picker cannot see for itself:
 *  whether a model created here would have a credential to DOWNLOAD its weights
 *  with. */
export type ModelSearchResult = {
  models: ModelCard[];
  /** Whether the control plane holds a usable HUGGING_FACE_HUB_TOKEN for this
   *  target — the token the agent injects so the runtime can pull the weights,
   *  NOT the CP_HUGGING_FACE_TOKEN its own metadata calls use. The wire name is
   *  unchanged and its meaning is not: it used to report the picker's token,
   *  which answered a question nobody was asking (see gatedWarning). False also
   *  covers "we could not confirm one", so it warns rather than blocks. */
  tokenConfigured: boolean;
  /** Set when the lookup itself failed — a timeout, an unreachable control
   *  plane. Distinct from "no matches", which is `models: []` and no error,
   *  because only one of the two is worth a Retry button. */
  error?: string;
};

/** The answer to "will this model start on that host". */
export type ModelFit = {
  fits: boolean;
  /** Both numbers, in the operator's words. Only set when fits is false. */
  reason?: string;
};

/** Just enough of a host to answer the fit question. Typed as a slice of
 *  HostFacts rather than the whole blob because that is all the deploy target
 *  carries — see DeployTargetServer.gpu. */
export type GpuHostFacts = { gpu?: HostFacts["gpu"] };

/**
 * Does this model fit ONE of that server's cards?
 *
 * Per card, never the total, and that is not a simplification: the engine
 * catalog renders no --tensor-parallel-size, so vLLM loads the whole model into
 * a single card's memory. A 2 × 24 GB host runs 24 GB models, not 48 GB ones,
 * and summing the cards would promise a deploy the runtime cannot perform. It is
 * also the basis the control plane's create-time check compares against
 * (store.checkModelFits), and the two walls agreeing matters more than either
 * being clever.
 *
 * Everything unknown FAILS OPEN, in both directions:
 *
 *   - An unsized model (parametersKnown false) is never blocked. The size would
 *     be a guess, and refusing someone's own model on their own hardware over a
 *     guessed number is the one thing this feature must never do.
 *   - A host that reported no GPU inventory is never filtered out. Absent is
 *     UNKNOWN, never empty — the rule the registration gate holds (see
 *     server-compat.ts) and the reason an agent too old to report facts does not
 *     take a fleet's worth of GPU servers out of the deploy flow.
 */
export function serverFitsModel(
  card: ModelCard | null | undefined,
  facts: GpuHostFacts | null | undefined,
  /** How the refusal names the hardware it just measured. Prose only — it
   *  changes no arithmetic and no verdict. A cluster row says "this cluster's
   *  largest GPU node" because "this server's GPU" under a row badged Cluster
   *  reads as a bug in the picker rather than a fact about the fleet, and the
   *  alternative — a second fit function for clusters — is how the two targets
   *  would come to answer the same question differently. */
  hardware = "this server's GPU"
): ModelFit {
  if (!card || !card.parametersKnown || card.vramBytesRequired <= 0) return { fits: true };
  const perGpu = facts?.gpu?.vramBytesPerGpu ?? 0;
  if (perGpu <= 0) return { fits: true };
  if (card.vramBytesRequired <= perGpu) return { fits: true };
  return {
    fits: false,
    reason: `This model needs about ${vramNeedText(card)} of VRAM; ${hardware} has ${formatReportedBytes(
      perGpu
    )}.`,
  };
}

/**
 * The model's requirement as prose, from the string the control plane already
 * rendered.
 *
 * vramText arrives as "~21 GB" and the tilde is dropped because every sentence
 * that uses this already says "about" — the NUMBER is still the control plane's,
 * and that is the part that must not be recomputed here. Exported so the picker
 * and the refusal share one phrasing rather than each stripping the tilde their
 * own way. The fallback covers a control plane that sent a byte count with no
 * phrase (an older one, or one whose renderer changed): a refusal has to state a
 * size, and the renderer the CP uses for the GPU's own capacity is the closest
 * thing to its answer.
 */
export function vramNeedText(card: ModelCard): string {
  const text = card.vramText.trim().replace(/^~\s*/, "");
  return text || formatReportedBytes(card.vramBytesRequired);
}

/**
 * What is worth SAYING about a gated model with no weights token, or null.
 *
 * A warning, not a refusal, and the difference is the whole point. A gated
 * repository serves its weights only to an account that has been granted
 * access, and `tokenConfigured` tells us whether this control plane holds a
 * HUGGING_FACE_HUB_TOKEN to pull them with — which is not the same as whether
 * the operator HAS access. They may have accepted the licence months ago and
 * hold a token in a secret this process cannot read, and blocking Continue on
 * our guess would leave them on a step with no way forward and nothing to fix.
 * Warning costs a paragraph; blocking costs them the feature.
 *
 * It names HUGGING_FACE_HUB_TOKEN because that is the variable the runtime reads
 * on the GPU host (store.HubTokenSecretName). CP_HUGGING_FACE_TOKEN — which this
 * sentence used to name — authenticates the picker's metadata calls and would
 * not have helped a single failing pull.
 */
export function gatedWarning(
  card: ModelCard | null | undefined,
  tokenConfigured: boolean
): string | null {
  if (!card?.gated || tokenConfigured) return null;
  return `${card.id} is gated — Hugging Face serves its weights only to an account that has been granted access, and no HUGGING_FACE_HUB_TOKEN is configured here. Accept the model's licence on huggingface.co and set HUGGING_FACE_HUB_TOKEN on the control plane to a token from that account, or the first pull fails with a 401. Continue if you have already done both — this check cannot see your account.`;
}

/** The Hub's task for the models an endpoint can serve, spelled exactly as the
 *  control plane spells it (hf.TextGenerationTask): the search asks the Hub for
 *  this task and the refusal below compares against it, so one string decides
 *  both. */
const TEXT_GENERATION_TASK = "text-generation";

/**
 * Why this repository cannot be served AT ALL, or null.
 *
 * Both branches describe a deploy that succeeds and then does nothing, which is
 * the worst failure this product has: the container starts, the health check
 * passes, the resource reads green, and every request 404s or the runtime
 * crash-loops on a host billed at GPU rates. Nothing downstream catches it —
 * there is no probe for "serving the wrong thing" — so the model step, where the
 * pick is still on screen, is the only place it can be caught.
 *
 *   - GGUF is an ollama format and vLLM cannot load it. The control plane no
 *     longer derives ollama from it (hf.EngineForModel says why: the derived
 *     runtime could not resolve a Hub repo id either), so a GGUF card would
 *     deploy as vLLM and serve nothing.
 *   - A pipeline tag that is not text generation is whisper, an embedding model
 *     or a diffusion model. openai/whisper-large-v3 sizes cleanly at ~4 GB, fits
 *     every card in the fleet, and crash-loops vLLM.
 *
 * An EMPTY pipeline tag is unknown and never refused: the Hub returns no
 * metadata at all for a gated repository read without a token, and refusing on
 * silence would refuse every gated model on a control plane that has no picker
 * token — the one case where the operator can do least about it.
 */
export function unservableReason(card: ModelCard | null | undefined): string | null {
  if (!card) return null;
  if (card.quantization.trim().toLowerCase() === "gguf") {
    return `${card.id} is a GGUF build, and the runtime this control plane deploys cannot load GGUF weights — pick the original safetensors repository for this model instead.`;
  }
  const tag = card.pipelineTag.trim();
  if (tag && tag !== TEXT_GENERATION_TASK) {
    return `${card.id} is a ${tag} model, and a model endpoint serves text generation only — this one would start, report healthy and fail every request. Pick a text-generation repository.`;
  }
  return null;
}

/**
 * Whether the model step may be left — the whole canContinue gate for it.
 *
 * It blocks on certainties only: nothing has been named, or the named model is
 * one no runtime this control plane deploys could serve. A gated model is NOT
 * one of them — see gatedWarning for why we cannot prove the operator lacks
 * access, and why a warning they can read beats a wall they cannot pass.
 *
 * It also deliberately does NOT block a model that failed to resolve. A repo id
 * the Hub could not confirm may still be perfectly deployable — the Hub was
 * slow, the control plane has no catalogue configured at all (a supported,
 * minimal deployment), or the reference is an Ollama library tag, which names
 * nothing on the Hub by design. The control plane's resolve route answers 404
 * for all of those and its create path uses the id exactly as typed; a wizard
 * that refused to continue would be the only component in the stack treating
 * "unconfirmed" as "wrong", and it would make the picker's dependency on
 * huggingface.co a dependency of deploying at all.
 */
export function modelStepError(
  card: ModelCard | null | undefined,
  modelId: string
): string | null {
  if (!modelId.trim()) {
    return "Pick a model, or type its Hugging Face repo id (owner/name).";
  }
  return unservableReason(card);
}

/**
 * Whether a string is shaped like a Hub repo id, which is what the resolve route
 * can look up.
 *
 * Used to decide whether to OFFER a typed reference as a model, never to refuse
 * one: an Ollama tag ("llama3.2:3b") fails this and is still a legal model
 * reference. The same shape check exists control-plane side
 * (store.looksLikeHubRepoID) and for the same purpose — knowing, without
 * spending a round trip, whether the Hub is even the right place to ask.
 */
export function looksLikeRepoId(value: string): boolean {
  const [owner, name, ...rest] = value.trim().split("/");
  if (rest.length > 0 || !owner || !name) return false;
  return !/[\s:]/.test(owner) && !/[\s:]/.test(name);
}

/** Parameter count as the picker's one-line summary ("8.0B params"). Counts,
 *  not bytes: this is the model's own headline number and never enters the fit
 *  comparison, which uses vramBytesRequired only. */
export function parameterText(card: ModelCard): string {
  if (!card.parametersKnown || card.parameters <= 0) return "size unknown";
  if (card.parameters >= 1e9) return `${(card.parameters / 1e9).toFixed(1)}B params`;
  return `${Math.round(card.parameters / 1e6)}M params`;
}

/** Download counts run to eight digits and the picker has one line. */
export function downloadsText(downloads: number): string {
  if (downloads >= 1e6) return `${(downloads / 1e6).toFixed(1)}M downloads`;
  if (downloads >= 1e3) return `${Math.round(downloads / 1e3)}k downloads`;
  return `${downloads} downloads`;
}
