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
// is not entirely ours to use; each is documented at its declaration.
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
	// weight: the KV cache for an ordinary context window, activations for a
	// modest batch, and allocator fragmentation. 20% is the smallest headroom
	// that holds for the shapes people actually deploy; a long-context or
	// high-concurrency configuration needs more, and the runtime will say so at
	// start-up rather than us refusing an ordinary deploy to protect an
	// extraordinary one.
	KVActivationFactor = 1.20

	// UtilizationCap is vLLM's default --gpu-memory-utilization. The runtime
	// will not allocate the last 10% of the card, so sizing against 100% of the
	// card's memory would pass a model the runtime then refuses to start — a fit
	// check that says yes and a container that crash-loops is worse than no fit
	// check at all.
	UtilizationCap = 0.90
)

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
		if value <= 0 || value > 1e13 {
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
// "~21 GB".
//
// Decimal GB, not GiB, because the number the user is comparing it against is
// the one printed on the card ("a 24 GB 4090"), and a fit check that reports
// 22 GiB against a 24 GB card invites arithmetic nobody should have to do.
//
// Zero renders as the empty string rather than "~0 GB": an unsized model has no
// size to show, and a UI can test for empty. "~0 GB" is a number, and it lies.
func FormatVRAM(bytes uint64) string {
	if bytes == 0 {
		return ""
	}
	if bytes < 1e9 {
		return fmt.Sprintf("~%.0f MB", math.Max(1, math.Round(float64(bytes)/1e6)))
	}
	return fmt.Sprintf("~%.0f GB", math.Round(float64(bytes)/1e9))
}

// EngineForModel picks the runtime from the model's own metadata: GGUF weights
// are an ollama format and everything else is served by vLLM.
//
// This is a question the wizard used to ASK, on a screen before it knew anything
// about the model — and it was a question with exactly one correct answer that
// the user had to look up. A picked GGUF repository cannot be loaded by vLLM and
// a picked safetensors repository is not what ollama wants, so every answer but
// this one led to a container that would not start. The step is gone; the
// metadata answers it.
//
// The returned names are keys of the store package's engine catalog. They are
// literals here so this package stays free of the database layer; the API layer
// that renders the resource spec is where the two meet.
func EngineForModel(library, quantization string) string {
	if strings.EqualFold(strings.TrimSpace(quantization), "gguf") ||
		strings.EqualFold(strings.TrimSpace(library), "gguf") {
		return "ollama"
	}
	return "vllm"
}

// applySizing fills in everything a ModelCard derives from its own metadata. It
// is the only writer of Parameters, VRAMBytesRequired and VRAMText.
func applySizing(card *ModelCard, tags []string, dtype string, safetensorsTotal uint64) {
	card.Quantization = detectQuantization(card.ID, tags, dtype)
	card.Engine = EngineForModel(card.Library, card.Quantization)
	card.BytesPerParam = BytesPerParam(dtype, card.Quantization)

	switch {
	case safetensorsTotal > 0 && !packsParameters(card.Quantization):
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
