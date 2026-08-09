package store

// The create-time model re-checks (SIGMA-213, SIGMA-214): can we serve this
// repository at all, and does it fit the card.
//
// The wizard already tells the operator, before they click Create, that a 70B
// model will not fit a 24 GB card and that a GGUF repository is not something
// vLLM opens. This file is the same answer on the API boundary, and it exists
// because the wizard is a courtesy while this is the rule: an API-direct caller,
// a script, a replayed request or a dashboard built against last month's
// contract must hit the same wall, or the guarantee the wizard makes is
// decoration. The failures it replaces are expensive and late — the resource is
// created, the reconciler renders it, the runtime pulls tens of gigabytes of
// weights onto a host billed at GPU rates, and then either CUDA reports an
// out-of-memory that names no model and suggests no fix, or worse, the container
// comes up HEALTHY and 404s every completion.
//
// EVERYTHING HERE FAILS OPEN, and that is the load-bearing design decision, not
// a caveat. The estimate is weights × dtype × a margin; it does not know the
// context window the operator will configure, whether the runtime will offload,
// or what a future quantization does to the arithmetic. Weighed against that,
// consider what a fail-CLOSED version costs: huggingface.co has an incident,
// and nobody in the world can deploy a model endpoint on their own hardware
// until it ends. So NOTHING is checked at all whenever
//
//   - the Hub could not be reached, or was too slow, or answered with something
//     unparsable, or
//   - the model reference is not a Hub repo id at all (an Ollama tag such as
//     `llama3.2:3b` names nothing on the Hub), or
//   - no sizer was ever wired, which is a supported control plane.
//
// Each check then drops out again on the fact IT needs. The fit check needs two
// numbers, so it skips when the parameter count is unknown (see
// hf.ModelCard.parametersKnown: neither the safetensors index nor the repo id
// yielded one — refusing on a guess is the one thing this must never do) or when
// the target host has never reported a GPU inventory, which is an agent older
// than SIGMA-201 or one whose nvidia probe failed this tick. The servability
// check needs a declared format or task, so an empty one — the shape a gated
// repository read without a token comes back in — refuses nothing.
//
// A refusal therefore requires a fact the Hub actually stated, and says it out
// loud along with the way out.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/hf"
)

// ModelSize is what the control plane worked out about one model reference, and
// it carries exactly the facts a CREATE decides on: can any runtime we render
// serve this repository, will it fit the card, and how long a context may it be
// started at.
//
// It is deliberately small, and it does NOT reuse hf.ModelCard. The store must
// not learn what a Hugging Face repo is, and a struct with fifteen fields would
// invite the next author to make a provisioning decision out of `gated`,
// `likes` or `sizingBasis` — presentation the picker owns.
//
// Quantization and PipelineTag are on the list because the wall they draw has
// to exist on the API boundary and not only in the browser. The wizard already
// refuses a GGUF repository and a non-text-generation task at the model step;
// with the same rules living nowhere else, an API-direct create of
// TheBloke/phi-2-GGUF was ACCEPTED, and vLLM served a container that reported
// healthy and 404'd every completion on a host billed at GPU rates.
type ModelSize struct {
	// ParametersKnown is the ONLY field that decides whether a fit check
	// happens. False means the sizing was a guess or an absence, and a guess
	// never blocks a deploy.
	ParametersKnown bool
	// VRAMBytesRequired is the estimate in bytes, on the same basis as the card
	// the wizard rendered: weights × bytesPerParam × KV/activation factor ÷ the
	// runtime's utilization cap.
	VRAMBytesRequired uint64
	// VRAMText is that figure already rendered ("~21.4 GB"). Carried rather than
	// re-derived so the refusal quotes character-for-character what the picker
	// showed — an operator comparing the two screens must not find two spellings
	// of one number and wonder which is real.
	VRAMText string
	// Quantization is the storage format the Hub reported ("gguf", "awq",
	// "none"). Empty means the lookup produced nothing, which is UNKNOWN and
	// never a refusal.
	Quantization string
	// PipelineTag is the Hub's task for the repository. Empty is UNKNOWN and
	// must stay permitted: a gated repository read without a token carries no
	// metadata at all, so reading empty as "not a text model" would make a
	// tokenless control plane refuse every gated repo there is.
	PipelineTag string
	// MaxPositionEmbeddings is the model's own context ceiling, 0 when the Hub
	// did not say. It is stored on the endpoint at provision so the runtime is
	// started inside a window the model actually has — see llm_engines.go's
	// Command, and hf.ServedContextTokens for what 0 instructs.
	MaxPositionEmbeddings int
}

// ModelSizer answers "what does this model reference need, and can we serve it".
// It is the store's whole view of huggingface.co.
//
// Injected rather than dialled directly, for the same reason InstallationTokenSource
// is: the store owns transactions and must not own an HTTP client. cmd/sigmahub-cp
// adapts the hf.Client to it at boot; a nil sizer (the default, and every test
// that does not care) simply means no fit check, which is the fail-open state
// described above.
type ModelSizer interface {
	SizeModel(ctx context.Context, repoID string) (ModelSize, error)
}

// SetModelSizer installs the SIGMA-214 sizer. Optional: resource creation works
// unchanged without it, minus the fit refusal.
func (s *Store) SetModelSizer(m ModelSizer) { s.modelSizer = m }

// modelSizeBudget bounds the Hub lookup a create waits on.
//
// The sizer runs BEFORE the transaction opens, so a slow Hub cannot hold a
// database transaction — but it can still make Create feel broken, and a
// wizard's Create button that hangs for thirty seconds is worse than one that
// skips an estimate. Past this budget the check is simply not performed, which
// is the same fail-open direction as every other unknown here.
const modelSizeBudget = 5 * time.Second

// llmSpecFields is the part of an `llm` resource's spec the control plane acts
// on. Decoded in one place because two decoders is how the provisioning path
// and the fit check would end up disagreeing about which model was requested —
// and the fit check refusing a model the runtime was never going to load is the
// worst possible version of this feature.
type llmSpecFields struct {
	Engine string `json:"engine"`
	Model  string `json:"model"`
}

// parseLLMSpec decodes the llm spec. A spec that will not parse yields the zero
// value, whose fields already mean "the caller said nothing" — the insert path
// then applies the default engine and an empty model, exactly as before.
func parseLLMSpec(spec json.RawMessage) llmSpecFields {
	var out llmSpecFields
	if len(spec) > 0 {
		_ = json.Unmarshal(spec, &out)
	}
	return out
}

// looksLikeHubRepoID reports whether a model reference is an owner/name repo id
// on huggingface.co, which is the only thing the sizer can look up.
//
// The check is not pedantry about format: `llm` resources can run on Ollama,
// whose model references are library tags like `llama3.2:3b` or `mistral`. Those
// name nothing on the Hub, so sizing them would spend a round trip to earn a
// 404 on every single Ollama create — and a 404 that always happens is a check
// that has stopped meaning anything.
//
// It is NOT a duplicate of the Hub client's own id validation and must not be
// deleted in favour of it: that one decides whether a request to huggingface.co
// is well-formed, this one decides whether the reference is a Hub reference at
// all. Only the second question can be answered without the round trip, which
// is the entire point.
func looksLikeHubRepoID(model string) bool {
	owner, name, ok := strings.Cut(model, "/")
	if !ok || owner == "" || name == "" {
		return false
	}
	// A second slash is a path (a file inside a repo), and a colon is a tag —
	// neither is a repo id, and both are shapes an operator plausibly pastes.
	return !strings.ContainsAny(name, "/: ") && !strings.ContainsAny(owner, ": ")
}

// sizeModelForFit resolves the requested model's size, or reports "unknown".
//
// Never returns an error: every failure mode — no sizer configured, not a Hub
// repo id, Hub down, Hub slow, model withdrawn — is the same outcome here, a
// zero ModelSize whose ParametersKnown is false and which therefore blocks
// nothing. Callers that treated an error as a reason to refuse would reintroduce
// precisely the outage this file's header rules out.
func (s *Store) sizeModelForFit(ctx context.Context, spec json.RawMessage) ModelSize {
	if s.modelSizer == nil {
		return ModelSize{}
	}
	model := strings.TrimSpace(parseLLMSpec(spec).Model)
	if !looksLikeHubRepoID(model) {
		return ModelSize{}
	}
	ctx, cancel := context.WithTimeout(ctx, modelSizeBudget)
	defer cancel()
	size, err := s.modelSizer.SizeModel(ctx, model)
	if err != nil {
		return ModelSize{}
	}
	return size
}

// checkModelServable refuses the two model shapes no runtime this control plane
// renders can actually serve. nil means the create proceeds.
//
// Both were refused ONLY in the browser, and domain.go states why that is not
// enough: "an API-direct create hits the wall the wizard draws" is the whole
// reason the VRAM check was duplicated CP-side, and these two belong to the same
// rule. Since EngineForModel returns "vllm" for every pick, an API-direct create
// of TheBloke/phi-2-GGUF was accepted and vLLM started a container that reported
// HEALTHY and 404'd every completion — the most expensive failure shape there
// is, because nothing in the product watches for it and the host is billed at
// GPU rates until somebody notices.
//
// It fails open on exactly the grounds checkModelFits does, and by the same
// mechanism rather than a parallel one: an unresolvable model yields the zero
// ModelSize, whose empty Quantization is not "gguf" and whose empty PipelineTag
// is UNKNOWN. A Hub outage therefore refuses nothing here either.
func checkModelServable(model string, size ModelSize) error {
	if size.Quantization == "gguf" {
		return ErrInvalid{Msg: fmt.Sprintf(
			"%s is a GGUF repository, and the vLLM runtime SigmaHub renders cannot load one — "+
				"the container starts, reports healthy, and answers 404 to every completion. "+
				"Pick this model's safetensors repository instead, or an AWQ or GPTQ 4-bit build "+
				"if you chose GGUF to save VRAM",
			model)}
	}
	// Empty is UNKNOWN and stays permitted: a gated repository resolved without
	// a token carries no metadata at all, so reading empty as "not a text model"
	// would make a control plane with no Hub token refuse every gated repo —
	// the one case where the operator can do least about it.
	if size.PipelineTag != "" && size.PipelineTag != hf.TextGenerationTask {
		return ErrInvalid{Msg: fmt.Sprintf(
			"%s is a %s model on the Hub, and an `llm` resource serves %s over an "+
				"OpenAI-compatible API — nothing here would answer /v1/completions for it. "+
				"Pick a %s model, or deploy this one as an `app` with a runtime image of your own",
			model, size.PipelineTag, hf.TextGenerationTask, hf.TextGenerationTask)}
	}
	return nil
}

// checkModelFits is the comparison itself: nil means the create proceeds.
//
// The capacity it compares against is VRAM PER CARD, not the host's total. That
// looks stricter than necessary on a two-GPU box and is correct: the engine
// catalog's Command() renders no --tensor-parallel-size, so vLLM loads the whole
// model into one card's memory and a 2 × 24 GB host runs 24 GB models, not 48 GB
// ones. It is also the same basis the wizard compares against
// (facts.gpu.vramBytesPerGpu, which is the SMALLEST card's figure), and the two
// walls agreeing matters more than either being clever — an operator who was
// told "this fits" by the dashboard must not then be refused by the API.
//
// target names the thing that has the memory, so the sentence reads as the
// operator's own inventory: "gpu-hel-01 has 24 GB per GPU".
func checkModelFits(model string, size ModelSize, vramBytesPerGPU uint64, target string) error {
	// Fail open: an unsized model is a guess, and a guess never blocks a deploy.
	if !size.ParametersKnown || size.VRAMBytesRequired == 0 {
		return nil
	}
	// Fail open: the host has not said what it has. Absent is unknown, not zero
	// — the same rule the registration gate holds (see compat.go's rule 1).
	if vramBytesPerGPU == 0 {
		return nil
	}
	if size.VRAMBytesRequired <= vramBytesPerGPU {
		return nil
	}
	need := size.VRAMText
	if need == "" {
		// A sizer that reported bytes but no sentence still has to produce one,
		// and it keeps the "~" the picker uses: the figure is an estimate, and a
		// refusal that states it as an exact number invites an argument about the
		// last gigabyte.
		need = "~" + humanBytes(size.VRAMBytesRequired)
	}
	// Both numbers, then the three real ways out — a refusal that only reports
	// the failure makes the operator invent the remedy, and the remedy here is
	// not obvious unless you already know that 4-bit builds exist.
	return ErrInvalid{Msg: fmt.Sprintf(
		"%s needs %s of VRAM but %s has %s per GPU — pick a smaller model, "+
			"pick a quantized build of this one (an AWQ or GPTQ 4-bit repo needs roughly a quarter "+
			"as much), or run it on a machine with a bigger card",
		model, need, target, humanBytes(vramBytesPerGPU))}
}
