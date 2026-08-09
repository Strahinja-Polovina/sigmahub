/**
 * The model step's verdicts: does this model fit that GPU, and can this control
 * plane even download it (SIGMA-213, SIGMA-214).
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

/** What a search answered, plus the one fact that changes what an EMPTY result
 *  means: without a Hub token the results are public repositories only, so a
 *  gated model the operator can see on huggingface.co is simply not here. */
export type ModelSearchResult = {
  models: ModelCard[];
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
  facts: GpuHostFacts | null | undefined
): ModelFit {
  if (!card || !card.parametersKnown || card.vramBytesRequired <= 0) return { fits: true };
  const perGpu = facts?.gpu?.vramBytesPerGpu ?? 0;
  if (perGpu <= 0) return { fits: true };
  if (card.vramBytesRequired <= perGpu) return { fits: true };
  return {
    fits: false,
    reason: `This model needs about ${vramNeedText(card)} of VRAM; this server's GPU has ${formatReportedBytes(
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
 * Why a gated model cannot be deployed by this control plane, or null.
 *
 * A gated repository serves its weights only to an account that has been granted
 * access. With no Hub token the pull fails with a 401 — after the resource
 * exists, on a host already billed at GPU rates — so this is refused at the
 * MODEL step, which is the last screen where the answer is still a different
 * model rather than a support ticket. The fix is an operator action on the
 * control plane, so the sentence names the environment variable: nobody guesses
 * "CP_HUGGING_FACE_TOKEN" from "access denied".
 */
export function gatedBlocked(
  card: ModelCard | null | undefined,
  tokenConfigured: boolean
): string | null {
  if (!card?.gated || tokenConfigured) return null;
  return `${card.id} is a gated repository — Hugging Face serves its weights only to an account that has been granted access, and this control plane holds no Hub token. Set CP_HUGGING_FACE_TOKEN on the control plane to a token from an account with access to this model, or pick an ungated one.`;
}

/**
 * Whether the model step may be left — the whole canContinue gate for it.
 *
 * It blocks on exactly two things, and both are certainties rather than
 * suspicions: nothing has been named, or the named model is one whose weights
 * this control plane provably cannot download.
 *
 * It deliberately does NOT block a model that failed to resolve. A repo id the
 * Hub could not confirm may still be perfectly deployable — the Hub was slow,
 * the control plane has no catalogue configured at all (a supported, minimal
 * deployment), or the reference is an Ollama library tag, which names nothing on
 * the Hub by design. The control plane's resolve route answers 404 for all of
 * those and its create path uses the id exactly as typed; a wizard that refused
 * to continue would be the only component in the stack treating "unconfirmed" as
 * "wrong", and it would make the picker's dependency on huggingface.co a
 * dependency of deploying at all.
 */
export function modelStepError(
  card: ModelCard | null | undefined,
  query: string,
  tokenConfigured: boolean
): string | null {
  if (!query.trim()) {
    return "Pick a model, or type its Hugging Face repo id (owner/name).";
  }
  return gatedBlocked(card, tokenConfigured);
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
