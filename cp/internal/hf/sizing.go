package hf

// Model sizing (SIGMA-214).
//
// How much GPU memory a model needs is one arithmetic expression, and it lives
// here ONCE. The dashboard receives the byte count and compares it to the GPU it
// was told about; it does not re-derive it, because the moment two sides compute
// the same number the two sides can disagree — and the disagreement surfaces as
// a deploy that was promised on one screen and refused on the next.
//
// The formula:
//
//	weights  = parameters × bytesPerParam
//	required = weights × KVActivationFactor ÷ UtilizationCap
//
// Both constants exist because the weights are not the whole story and the card
// is not entirely ours to use; each is documented at its declaration. A third,
// SizedContextTokens, does not appear in the expression and is the reason the
// expression is true at all: the KV term budgets ONE context length, and the
// runtime has to be started at that length or shorter (see ServedContextTokens)
// or this arithmetic describes a deployment nobody made.
//
// The estimate is deliberately a little pessimistic, and it errs in the safe
// direction ONLY when the parameter count is real. When it is a guess there is
// no estimate at all — see applySizing.

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

const (
	// KVActivationFactor covers everything resident on the card that is not a
	// weight: the KV cache, activations for a modest batch, and allocator
	// fragmentation. 20% is a measured number, not a feeling. Llama-3.1-8B — a
	// grouped-query checkpoint, which is what open weights have shipped as since
	// Llama-3 — spends 128 KiB of KV per token, so SizedContextTokens costs
	// 1.07 GB of the 3.21 GB this factor adds to its 16.06 GB of weights: the
	// pinned window two or three times over for a batch, and the rest for
	// activations and fragmentation.
	//
	// The number this factor buys is therefore a context length, not the model's
	// maximum, and a runtime started at any other length is running a
	// configuration nothing checked. That is what SizedContextTokens is for.
	KVActivationFactor = 1.20

	// SizedContextTokens is the context window the formula above is arithmetic
	// FOR, and so the CEILING on the window the runtime may be pinned to —
	// vLLM's --max-model-len. It is not the flag's value on its own: a model
	// whose own maximum is shorter is pinned to that instead, because vLLM
	// refuses to start otherwise. ServedContextTokens is the whole decision, and
	// the store renders the flag from it, never from this constant.
	//
	// It is the half of SIGMA-214 the first cut left out. With no --max-model-len
	// vLLM takes the model's max_position_embeddings, and for Llama-3.1 that is
	// 131072 tokens — sixteen times what KVActivationFactor budgets. On a 24 GiB
	// card the fit check compared 21.41 GB required against 25.77 GB available,
	// approved the deploy and drew a green checkmark; vLLM then asked for 131072
	// tokens of KV cache, found room for about 54k, and exited with "The model's
	// max seq len is larger than the maximum number of tokens that can be stored
	// in KV cache". A fit check that passes in front of the exact failure it
	// exists to prevent is worse than no fit check, because it also tells the
	// operator the problem is somewhere else.
	//
	// 8192 is long enough for the chat and retrieval shapes people deploy and
	// short enough that the 20% headroom is real at 8B on a 24 GB card, which is
	// the machine this feature was designed against. It is EXPORTED because the
	// start command the store renders derives from it: a literal there and a
	// factor here drift apart on the first change to either, and the drift is
	// invisible until a container exits.
	SizedContextTokens = 8192

	// UtilizationCap is vLLM's default --gpu-memory-utilization. The runtime
	// will not allocate the last 10% of the card, so sizing against 100% of the
	// card's memory would pass a model the runtime then refuses to start — a fit
	// check that says yes and a container that crash-loops is worse than no fit
	// check at all.
	UtilizationCap = 0.90

	// FormatMBCeilingMB and FormatTenthCeilingGB are FormatVRAM's two band
	// boundaries: below the first the estimate is rendered in whole megabytes,
	// below the second in tenths of a gigabyte, and at or above it in whole
	// gigabytes rounded up. They are named rather than written into the
	// expression because the dashboard's demo mode renders the same string with
	// no control plane to ask, from a generated copy of these numbers — see
	// store.RenderTypeScript. A literal here and a literal there is the drift
	// SIGMA-279 was: demo fixtures evaluated by hand, once, from constants that
	// then moved underneath them.
	FormatMBCeilingMB    = 1000
	FormatTenthCeilingGB = 100
)

// ServedContextTokens is the context window an endpoint for this model may
// actually be started at: the smaller of what the sizing paid for
// (SizedContextTokens) and what the model itself allows
// (maxPositionEmbeddings, as carried on ModelCard). It is the number the store
// renders into vLLM's --max-model-len.
//
// The clamp is not politeness. vLLM's _get_and_verify_max_len RAISES on a
// window longer than the model's own:
//
//	ValueError: User-specified max_model_len (8192) is greater than the derived
//	max_model_len (max_position_embeddings=2048 ...). To allow overriding this
//	maximum, set VLLM_ALLOW_LONG_MAX_MODEL_LEN=1
//
// Pinning every endpoint to SizedContextTokens therefore killed precisely the
// models that fit best. TinyLlama-1.1B-Chat (2048 tokens) is the catalogue's
// "fits anything" pick — 2.9 GB against a 40 GiB card, a green tick on every
// screen — and Llama-2-13B-chat-AWQ (4096) is its quantized one; both were
// approved by the fit check and both exited at startup on a host billed at GPU
// rates. That is SIGMA-214's own failure inverted: long-context models used to
// die, and then short-context ones did.
//
// Clamping DOWN cannot disturb the VRAM estimate. The KV term budgets
// SizedContextTokens of cache, and a window shorter than that spends less of
// it, so a clamped endpoint leaves the estimate MORE conservative — the one
// direction a fit check is allowed to be wrong in.
//
// An UNKNOWN ceiling falls back to SizedContextTokens, and this is the third
// answer to the same question — the first two were both wrong, in opposite
// directions, so the reasoning is worth stating fully.
//
// Pinning everything to 8192 killed short-context models. Rendering no flag at
// all killed long-context ones, and worse: it made the estimate and the runtime
// disagree exactly when nothing could check them, because the VRAM formula
// budgets a SizedContextTokens KV term UNCONDITIONALLY while an unpinned vLLM
// takes 131072. The fit check would still draw green.
//
// The asymmetry that settles it is which models land in the unknown branch.
// A ceiling is unknown when config.json could not be read, and that is the
// GATED repositories — Llama and its kin, whose ceilings are far above 8192, so
// clamping them is exactly right. A model whose real ceiling is BELOW 8192 is a
// small, ungated one whose config.json reads fine, so it never arrives here. And
// the two failures are not equal even when the guess is wrong: a window pinned
// too high is a container that never starts, while one pinned too low is an
// endpoint that serves shorter prompts than it could — visible, recoverable, and
// not a crash loop on a GPU-billed host.
//
// Zero is therefore never returned. The store's render-time guard against a
// zero still stands, for rows written before any of this existed.
func ServedContextTokens(maxPositionEmbeddings int) int {
	if maxPositionEmbeddings <= 0 {
		return SizedContextTokens
	}
	return min(SizedContextTokens, maxPositionEmbeddings)
}

// maxPlausibleParameters is where a parameter count stops being a parameter
// count. Ten trillion is an order of magnitude past the largest model anyone has
// published, so a value above it is a version string we misread or a Hub field
// that is wrong — and either one has to be REJECTED rather than sized, because
// both reach the same place: a count that large overflows the byte arithmetic
// and renders as "~9223372037 GB", which is a fit check gating a deploy on a
// number that means nothing.
//
// It applies to BOTH sources of a count. The name path has always had it; the
// safetensors path is a third party's integer and had none.
const maxPlausibleParameters = 1e13

// paramPattern finds a size token in a repository name: "7B", "1.5B", "405B",
// "135M", and the mixture-of-experts spelling "8x7B". The surrounding boundary
// checks in ParseParameterCount are what keep it from firing on "4bit".
var paramPattern = regexp.MustCompile(`(?i)(?:([0-9]+)x)?([0-9]+(?:\.[0-9]+)?)([bm])`)

// ParseParameterCount reads a parameter count out of a repository id. It is the
// fallback for repositories the Hub reports no safetensors index for — GGUF
// conversions, gated repositories we cannot read, anything published without the
// index — and it is the ONLY thing standing between those models and having no
// size at all.
//
// Only the last path segment is searched: the size belongs to the model name,
// and an owner like "01-ai" has no business contributing digits to it.
//
// The LARGEST candidate wins, not the last one. "Qwen2.5-7B-Instruct-1M" is a
// real repository whose trailing token is a context length, and reading it
// right-to-left would size a 7-billion-parameter model at one million.
func ParseParameterCount(repoID string) (uint64, bool) {
	name := repoID
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.ToLower(name)

	var best uint64
	for _, m := range paramPattern.FindAllStringSubmatchIndex(name, -1) {
		start, end := m[0], m[1]
		// A size token stands alone. Without these two checks "4bit" reads as
		// 4 billion parameters and "v2b3" as 2 billion, and both appear in real
		// repository names.
		if start > 0 && isNameWordByte(name[start-1]) {
			continue
		}
		if end < len(name) && isNameWordByte(name[end]) {
			continue
		}

		value, err := strconv.ParseFloat(name[m[4]:m[5]], 64)
		if err != nil {
			continue
		}
		// "8x7B" is eight experts of seven billion each. The full weight set has
		// to be resident even though only two experts run per token, so the
		// product — not the 7B — is what has to fit.
		if m[2] >= 0 {
			experts, err := strconv.ParseFloat(name[m[2]:m[3]], 64)
			if err != nil || experts == 0 {
				continue
			}
			value *= experts
		}
		switch name[m[6]] {
		case 'b':
			value *= 1e9
		default:
			value *= 1e6
		}
		// Reject the absurd rather than gate a deploy on it: a name that claims
		// ten trillion parameters is a version string we misread.
		if value <= 0 || value > maxPlausibleParameters {
			continue
		}
		if n := uint64(math.Round(value)); n > best {
			best = n
		}
	}
	return best, best > 0
}

func isNameWordByte(ch byte) bool {
	return ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '.'
}

// BytesPerParam is how many bytes one parameter occupies on the card.
//
// Quantization wins over dtype when both are known, because a quantized
// repository still reports a floating-point dtype for the tensors it did not
// quantize (scales, embeddings, the odd unquantized layer) and sizing an AWQ
// checkpoint at two bytes per parameter would overstate it fourfold.
func BytesPerParam(dtype, quantization string) float64 {
	switch strings.ToLower(strings.TrimSpace(quantization)) {
	case "gguf":
		// An average, and unusually a deliberate one: a GGUF repository holds
		// several quant levels (Q3_K_S through Q8_0) and the repository metadata
		// does not tell us which one will be pulled. 0.6 is a Q4_K_M-ish middle
		// — the level people actually run — so the number is wrong for the
		// extremes in both directions by design.
		return 0.6
	case "awq", "gptq", "4bit", "int4":
		return 0.5
	case "fp8", "f8":
		return 1
	}
	switch strings.ToUpper(strings.TrimSpace(dtype)) {
	case "F64":
		return 8
	case "F32":
		return 4
	case "F16", "BF16":
		return 2
	}
	if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(dtype)), "F8") {
		// F8_E4M3 / F8_E5M2 — the dtype name carries the exponent layout.
		return 1
	}
	// Unknown dtype: assume BF16, which is what an open-weights repository
	// published in the last two years ships.
	return 2
}

// RequiredVRAMBytes applies the formula. A zero parameter count returns zero,
// and zero is the value that means "do not check" everywhere downstream — it is
// never a model that needs no memory.
func RequiredVRAMBytes(parameters uint64, bytesPerParam float64) uint64 {
	if parameters == 0 || bytesPerParam <= 0 {
		return 0
	}
	weights := float64(parameters) * bytesPerParam
	return uint64(math.Ceil(weights * KVActivationFactor / UtilizationCap))
}

// FormatVRAM renders a byte count as the one string both sides show, e.g.
// "~21.4 GB".
//
// Decimal GB, not GiB, because the number the user is comparing it against is
// the one printed on the card ("a 24 GB 4090"), and a fit check that reports
// 22 GiB against a 24 GB card invites arithmetic nobody should have to do.
//
// The tenth of a gigabyte below 100 GB is load-bearing, not decoration. The
// capacity this figure is set against is rendered by store.humanBytes and the
// web's formatReportedBytes, and both TRUNCATE — so a 17.33 GB requirement on a
// 17.18 GB card produced "needs ~17 GB but this server has 17 GB", a refusal
// that reads as a bug in the refusal and leaves the operator with nothing to
// act on. At or above 100 GB a tenth is noise, so the estimate rounds UP there
// instead; that keeps the same guarantee by a different route, because a figure
// that is larger and never rounds down cannot land on a figure that is smaller
// and always truncates.
//
// Zero renders as the empty string rather than "~0 GB": an unsized model has no
// size to show, and a UI can test for empty. "~0 GB" is a number, and it lies.
func FormatVRAM(bytes uint64) string {
	if bytes == 0 {
		return ""
	}
	// The unit is chosen from the ROUNDED figure rather than the raw byte count:
	// 999999999 bytes is under a gigabyte and rounds to 1000 MB, and "~1000 MB"
	// is a gigabyte spelled the long way.
	if mb := math.Round(float64(bytes) / 1e6); mb < FormatMBCeilingMB {
		return fmt.Sprintf("~%.0f MB", math.Max(1, mb))
	}
	gb := float64(bytes) / 1e9
	if gb < FormatTenthCeilingGB {
		return fmt.Sprintf("~%.1f GB", gb)
	}
	return fmt.Sprintf("~%.0f GB", math.Ceil(gb))
}

// EngineForModel is the runtime the control plane will render for a picked
// model. Every model picked here gets vLLM.
//
// The question was one the wizard used to ASK, on a screen before it knew
// anything about the model, and it is not being given back. What changed is the
// number of answers: it used to have two, and the second one did not work.
//
// A GGUF repository derived "ollama", whose spec carries the model in
// OLLAMA_MODEL and no start command — but ollama cannot resolve
// "TheBloke/phi-2-GGUF". It wants hf.co/<id> or a library tag, so it pulled
// nothing, the container came up HEALTHY, and every completion 404'd. A runtime
// that starts and serves nothing is worse than one that refuses to start,
// because nothing in the product is watching for it and the operator is billed
// at GPU rates while they find out. So a GGUF repository is refused at the model
// step instead of being routed to a runtime that cannot serve it; the card keeps
// its Quantization of "gguf", which is how the picker recognises the pick and
// says so.
//
// ollama remains a supported ENGINE in the store's catalog and that path still
// works: a hand-entered library tag ("llama3.1:8b") is a reference ollama CAN
// resolve. It is only no longer DERIVED, because deriving it produced an
// endpoint that served nothing.
//
// The arguments stay because they are the record of what used to decide this,
// and because this is still the one place the question is answered — the next
// person who wants an engine derived from a model's format has to come here to
// do it, which is where the paragraph above is.
//
// The returned name is a key of the store package's engine catalog. It is a
// literal here so this package stays free of the database layer; the API layer
// that renders the resource spec is where the two meet.
func EngineForModel(library, quantization string) string {
	return "vllm"
}

// applyRuntime fills in what follows from the repository's FORMAT rather than
// its size: how it is stored, what would serve it, and what one parameter of it
// costs on the card.
//
// It is separate from applySizing because the two questions have different
// answers when the Hub refuses to describe a repository at all — a gated model
// read without a token still gets a runtime, and does NOT get a size (see
// gatedCard).
func applyRuntime(card *ModelCard, tags []string, dtype string) {
	card.Quantization = detectQuantization(card.ID, tags, dtype)
	card.Engine = EngineForModel(card.Library, card.Quantization)
	card.BytesPerParam = BytesPerParam(dtype, card.Quantization)
}

// applySizing fills in everything a ModelCard derives from its own metadata. It
// is the only writer of Parameters, VRAMBytesRequired and VRAMText.
func applySizing(card *ModelCard, tags []string, dtype string, safetensorsTotal uint64) {
	applyRuntime(card, tags, dtype)

	switch {
	// The upper bound is the one the name path applies, for the same reason:
	// safetensors.total is a third party's integer, and a wrong one is not a
	// slightly wrong size, it is "~9223372037 GB" in front of a deploy. Over the
	// bound the name takes over, which is where a repository with no readable
	// index lands anyway.
	case safetensorsTotal > 0 && safetensorsTotal <= maxPlausibleParameters && !packsParameters(card.Quantization):
		card.Parameters, card.ParametersKnown, card.SizingBasis = safetensorsTotal, true, "safetensors"
	default:
		// The documented priority order puts safetensors.total first because it
		// is exact — except for AWQ and GPTQ, where it is not a parameter count
		// at all. Those formats pack eight 4-bit weights into one int32 tensor
		// element, and the Hub counts ELEMENTS, so a 7B AWQ checkpoint reports
		// roughly 1.1e9: multiplied by 0.5 bytes it would claim a 7B model fits
		// in well under a gigabyte, and the fit check would wave through a
		// deploy that cannot start. The name is the more truthful source for
		// exactly these two formats.
		if n, ok := ParseParameterCount(card.ID); ok {
			card.Parameters, card.ParametersKnown, card.SizingBasis = n, true, "name"
		} else {
			card.SizingBasis = "unknown"
		}
	}

	if !card.ParametersKnown {
		// Nothing is guessed here, and that is the rule: with no parameter count
		// there is no VRAM figure, and with no VRAM figure there is no fit check
		// anywhere in the product. A model we cannot size deploys — it is the
		// user's own hardware and their own model, and refusing it on a number
		// we invented would be the worst of both.
		card.Parameters, card.VRAMBytesRequired, card.VRAMText = 0, 0, ""
		return
	}
	card.VRAMBytesRequired = RequiredVRAMBytes(card.Parameters, card.BytesPerParam)
	card.VRAMText = FormatVRAM(card.VRAMBytesRequired)
}

// packsParameters reports whether a quantization stores several weights inside
// one tensor element, which is what makes the Hub's element count useless as a
// parameter count.
func packsParameters(quantization string) bool {
	switch quantization {
	case "awq", "gptq":
		return true
	default:
		// GGUF is packed too, but a GGUF repository has no safetensors index to
		// mis-read in the first place — and a dual-format repository's index
		// describes the unpacked safetensors, which IS the parameter count.
		return false
	}
}

// detectQuantization reads the storage format off the repository id, its tags
// and its dominant dtype, in that order of specificity.
func detectQuantization(repoID string, tags []string, dtype string) string {
	haystack := strings.ToLower(repoID)
	for _, t := range tags {
		haystack += " " + strings.ToLower(t)
	}
	switch {
	case strings.Contains(haystack, "gguf"):
		return "gguf"
	case strings.Contains(haystack, "awq"):
		return "awq"
	case strings.Contains(haystack, "gptq"):
		return "gptq"
	case strings.Contains(haystack, "fp8"), strings.HasPrefix(strings.ToUpper(strings.TrimSpace(dtype)), "F8"):
		return "fp8"
	case strings.Contains(haystack, "4bit"), strings.Contains(haystack, "4-bit"), strings.Contains(haystack, "int4"):
		// A 4-bit repository that names no vendor (bitsandbytes, mostly) is
		// reported as "awq". It is the closest of the five values the wire
		// contract defines, it sizes identically at half a byte per parameter,
		// and the alternative — a sixth value — would reach a dashboard that has
		// no badge for it. The number is what gates the deploy; the label only
		// names it.
		return "awq"
	}
	return "none"
}

// dominantDType picks the dtype the weights are actually stored in: the float
// dtype with the most elements.
//
// Integer and boolean entries are skipped on purpose. A safetensors index lists
// I64 position ids and BOOL attention masks alongside the weights, and they say
// nothing about how many bytes a parameter costs — on a small model with a large
// vocabulary they can outnumber the weight tensors' own entries.
func dominantDType(parameters map[string]uint64) string {
	var best string
	var bestCount uint64
	for dtype, count := range parameters {
		upper := strings.ToUpper(dtype)
		if !strings.HasPrefix(upper, "F") && !strings.HasPrefix(upper, "BF") {
			continue
		}
		// Map iteration order is random, so ties are broken by name to keep the
		// sizing of one repository from changing between two requests.
		if count > bestCount || (count == bestCount && best != "" && upper < best) {
			best, bestCount = upper, count
		}
	}
	return best
}
