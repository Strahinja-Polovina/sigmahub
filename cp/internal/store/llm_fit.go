package store

// The create-time VRAM fit re-check (SIGMA-214).
//
// The wizard already tells the operator, before they click Create, that a 70B
// model will not fit a 24 GB card. This file is the same answer on the API
// boundary, and it exists because the wizard is a courtesy while this is the
// rule: an API-direct caller, a script, a replayed request or a dashboard built
// against last month's contract must hit the same wall, or the guarantee the
// wizard makes is decoration. The failure it replaces is expensive and late —
// the resource is created, the reconciler renders it, the runtime pulls tens of
// gigabytes of weights onto a host billed at GPU rates, and CUDA reports an
// out-of-memory that names no model and suggests no fix.
//
// EVERYTHING HERE FAILS OPEN, and that is the load-bearing design decision, not
// a caveat. The estimate is weights × dtype × a margin; it does not know the
// context window the operator will configure, whether the runtime will offload,
// or what a future quantization does to the arithmetic. Weighed against that,
// consider what a fail-CLOSED version costs: huggingface.co has an incident,
// and nobody in the world can deploy a model endpoint on their own hardware
// until it ends. So there is no fit check at all whenever
//
//   - the model's parameter count is unknown (see hf.ModelCard.parametersKnown:
//     neither the safetensors index nor the repo id yielded a number — refusing
//     on a guess is the one thing this must never do), or
//   - the Hub could not be reached, or was too slow, or answered with something
//     unparsable, or
//   - the model reference is not a Hub repo id at all (an Ollama tag such as
//     `llama3.2:3b` names nothing on the Hub), or
//   - the target host has never reported a GPU inventory — an agent older than
//     SIGMA-201, or one whose nvidia probe failed this tick.
//
// A refusal therefore requires two independently KNOWN numbers, and says both
// of them out loud.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ModelSize is what the control plane worked out about one model reference: the
// VRAM its weights plus runtime overhead will need, and the sentence the
// dashboard is already showing for the same model.
//
// It is a deliberately tiny type, and it does NOT reuse hf.ModelCard. The store
// must not learn what a Hugging Face repo is — it needs a number and a phrase,
// and a struct with fifteen fields would invite the next author to make a store
// decision out of `gated` or `pipelineTag`, which belong to the picker.
type ModelSize struct {
	// ParametersKnown is the ONLY field that decides whether a fit check
	// happens. False means the sizing was a guess or an absence, and a guess
	// never blocks a deploy.
	ParametersKnown bool
	// VRAMBytesRequired is the estimate in bytes, on the same basis as the card
	// the wizard rendered: weights × bytesPerParam × KV/activation factor ÷ the
	// runtime's utilization cap.
	VRAMBytesRequired uint64
	// VRAMText is that figure already rendered ("~21 GB"). Carried rather than
	// re-derived so the refusal quotes character-for-character what the picker
	// showed — an operator comparing the two screens must not find two spellings
	// of one number and wonder which is real.
	VRAMText string
}

// ModelSizer answers "how much VRAM does this model reference need". It is the
// store's whole view of huggingface.co.
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
// target names the thing that has the memory, so the sentence works for both a
// server ("gpu-hel-01 has 24 GB per GPU") and a cluster, where the scheduler
// picks the node and the honest bound is its largest one.
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

// clusterNodeFactsTx reads the facts of every live node in a cluster, inside the
// caller's transaction so the membership it sees is the membership the resource
// is created against. A cluster with no nodes yet returns nothing, which sizes
// as unknown and skips the check — the right answer for a cluster that has been
// declared but not yet built out.
func clusterNodeFactsTx(ctx context.Context, tx pgx.Tx, clusterID string) ([]json.RawMessage, error) {
	rows, err := tx.Query(ctx, `
		SELECT sv.facts
		  FROM cluster_nodes n JOIN servers sv ON sv.id = n.server_id
		 WHERE n.cluster_id = $1 AND sv.deleted_at IS NULL`, clusterID)
	if err != nil {
		return nil, fmt.Errorf("read cluster node facts: %w", err)
	}
	defer rows.Close()
	var out []json.RawMessage
	for rows.Next() {
		var facts json.RawMessage
		if err := rows.Scan(&facts); err != nil {
			return nil, err
		}
		out = append(out, facts)
	}
	return out, rows.Err()
}

// maxVRAMPerGPU returns the largest per-card VRAM among a cluster's nodes.
//
// The MAXIMUM, not the minimum or the sum, because Kubernetes schedules the
// workload onto one node and it only has to fit somewhere: refusing a model
// that the cluster's one big GPU node could run, because a small node exists
// alongside it, would be a false refusal — and this check is allowed to be
// wrong only in the permissive direction. Nodes with no GPU facts contribute
// nothing, so a cluster nobody has reported hardware for sizes as unknown and
// the check is skipped entirely.
func maxVRAMPerGPU(nodeFacts []json.RawMessage) uint64 {
	var best uint64
	for _, raw := range nodeFacts {
		gpu := ParseHostFacts(raw).GPU
		if gpu == nil {
			continue
		}
		if gpu.VRAMBytesPerGPU > best {
			best = gpu.VRAMBytesPerGPU
		}
	}
	return best
}
