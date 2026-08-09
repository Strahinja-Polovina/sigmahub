package hf

import (
	"fmt"
	"strings"
	"testing"
)

// The number the whole feature turns on. Llama 3.1 8B in BF16 is the model the
// sizing was designed against, and if this drifts, every fit check in the
// product drifts with it.
func TestALlamaEightBillionInBF16SizesToTwentyOneGigabytes(t *testing.T) {
	const parameters = 8_030_261_248 // safetensors total from the real repository

	bpp := BytesPerParam("BF16", "none")
	if bpp != 2 {
		t.Fatalf("bytesPerParam = %v, want 2", bpp)
	}
	required := RequiredVRAMBytes(parameters, bpp)

	// weights 16.06 GB, ×1.20 for KV cache and activations, ÷0.90 because vLLM
	// will not touch the last tenth of the card.
	if required < 21_000_000_000 || required > 21_500_000_000 {
		t.Fatalf("required = %d bytes, want ~21.4e9", required)
	}
	if got := FormatVRAM(required); got != "~21.4 GB" {
		t.Fatalf("FormatVRAM = %q, want ~21.4 GB", got)
	}
}

// The fit check's headroom and the context length the runtime is started at are
// one decision written in two places, and this is the arithmetic that ties them
// together. The first cut of SIGMA-214 budgeted for an unnamed "ordinary context
// window" and pinned the runtime to nothing, so vLLM took the model's 131072 and
// exited on a deploy the fit check had approved. Raising SizedContextTokens
// without raising KVActivationFactor now fails here instead of in a container
// log an hour later.
func TestTheBudgetedHeadroomCoversTheContextTheRuntimeIsPinnedTo(t *testing.T) {
	// Llama-3.1-8B, the shape the sizing was designed against: 32 layers × 8 KV
	// heads × 128 head dimensions, a key and a value each, two bytes apiece —
	// 128 KiB of KV cache per token.
	const kvBytesPerToken = 2 * 32 * 8 * 128 * 2
	const parameters = 8_030_261_248

	weights := float64(parameters) * BytesPerParam("BF16", "none")
	headroom := weights*KVActivationFactor - weights
	kv := float64(SizedContextTokens) * kvBytesPerToken

	if kv > headroom {
		t.Fatalf("%d tokens of KV cache is %.2f GB but the factor budgets %.2f GB of headroom — "+
			"the fit check would approve a deploy the runtime cannot start",
			SizedContextTokens, kv/1e9, headroom/1e9)
	}
	// The other half of the same statement: the headroom is not KV cache alone.
	// Activations and allocator fragmentation live in what is left, so a context
	// length that eats the whole budget is as wrong as one that overruns it.
	if kv > headroom/2 {
		t.Fatalf("%d tokens of KV cache takes %.2f GB of %.2f GB, leaving nothing for activations "+
			"or fragmentation", SizedContextTokens, kv/1e9, headroom/1e9)
	}
}

// The window the runtime is started at is a negotiation with the model, not a
// number this package gets to pick on its own. vLLM's _get_and_verify_max_len
// RAISES when --max-model-len exceeds the model's own maximum —
//
//	ValueError: User-specified max_model_len (8192) is greater than the derived
//	max_model_len (max_position_embeddings=2048 ...)
//
// — so pinning every endpoint to SizedContextTokens killed the models that fit
// best: TinyLlama at 2048 tokens and Llama-2-13B-chat-AWQ at 4096 are the
// catalogue's easiest picks, both drew a green tick against every card in the
// product, and both exited at startup on a host billed at GPU rates.
func TestTheServedContextNeverExceedsTheModelsOwnMaximum(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		maxPositionEmbeddings int
		want                  int
	}{
		{"TinyLlama's 2048 is served whole", 2048, 2048},
		{"the 13B AWQ build's 4096 is served whole", 4096, 4096},
		{"Llama 3.1's 131072 is cut to the window the sizing paid for", 131072, SizedContextTokens},
		{"a model that ends exactly where the sizing does", SizedContextTokens, SizedContextTokens},
		{"one token short of the sized window", SizedContextTokens - 1, SizedContextTokens - 1},
		// An unknown ceiling gets the window the VRAM estimate was actually paid
		// for, so the flag and the arithmetic never disagree. Rendering nothing
		// here was the second wrong answer to this question: unknown is the
		// GATED repositories, whose ceilings are far ABOVE 8192, and leaving
		// them unpinned is the 131072-token KV-cache crash with a green fit
		// check in front of it.
		{"an unknown maximum falls back to the window that was estimated", 0, SizedContextTokens},
		{"a maximum that makes no sense is treated as unknown, not as short", -1, SizedContextTokens},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ServedContextTokens(tc.maxPositionEmbeddings)
			if got != tc.want {
				t.Fatalf("ServedContextTokens(%d) = %d, want %d", tc.maxPositionEmbeddings, got, tc.want)
			}
			// The other half of the same statement, and the reason clamping down
			// cannot disturb the estimate: the KV term budgets SizedContextTokens
			// of cache, and a served window is never longer than that, so a
			// clamped endpoint only leaves the VRAM figure more conservative.
			if got > SizedContextTokens {
				t.Errorf("served %d tokens against a KV budget bought for %d", got, SizedContextTokens)
			}
		})
	}
}

func TestParseParameterCountReadsTheSizeOutOfARepositoryName(t *testing.T) {
	for _, tc := range []struct {
		name   string
		repoID string
		want   uint64
		wantOK bool
	}{
		{"plain billions", "meta-llama/Llama-3.1-8B-Instruct", 8_000_000_000, true},
		{"the version prefix is not a size", "Qwen/Qwen2.5-7B-Instruct", 7_000_000_000, true},
		{"fractional", "TinyLlama/TinyLlama-1.1B-Chat-v1.0", 1_100_000_000, true},
		{"sub-billion", "Qwen/Qwen2-0.5B-Instruct", 500_000_000, true},
		{"millions", "HuggingFaceTB/SmolLM-135M", 135_000_000, true},
		{"the largest model there is", "meta-llama/Llama-3.1-405B-Instruct", 405_000_000_000, true},
		// Every expert's weights are resident even though two run per token.
		{"mixture of experts multiplies out", "mistralai/Mixtral-8x7B-Instruct-v0.1", 56_000_000_000, true},
		// The trailing token is a context length. Reading right-to-left would
		// size a seven-billion-parameter model at one million.
		{"a context-length suffix does not win", "Qwen/Qwen2.5-7B-Instruct-1M", 7_000_000_000, true},
		{"a quantization suffix is not a size", "unsloth/Llama-3.2-3B-Instruct-bnb-4bit", 3_000_000_000, true},
		{"the owner does not contribute digits", "01-ai/Yi-34B-Chat", 34_000_000_000, true},
		{"no size in the name", "google-bert/bert-base-uncased", 0, false},
		{"a version is not a size", "openai/whisper-large-v3", 0, false},
		{"a context length alone is not a size", "microsoft/phi-3-mini-4k-instruct", 0, false},
		{"nothing at all", "", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseParameterCount(tc.repoID)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("ParseParameterCount(%q) = %d, %v; want %d, %v", tc.repoID, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestBytesPerParamFollowsTheStorageFormat(t *testing.T) {
	for _, tc := range []struct {
		name         string
		dtype        string
		quantization string
		want         float64
	}{
		{"full precision", "F32", "none", 4},
		{"half precision", "F16", "none", 2},
		{"bfloat", "BF16", "none", 2},
		{"eight-bit float", "F8_E4M3", "fp8", 1},
		{"awq is four-bit", "F16", "awq", 0.5},
		{"gptq is four-bit", "F16", "gptq", 0.5},
		{"gguf is an average over quant levels", "", "gguf", 0.6},
		// A quantized repository still reports a float dtype for the tensors it
		// did not quantize; taking the dtype would overstate it fourfold.
		{"quantization outranks the reported dtype", "F32", "awq", 0.5},
		{"an unknown dtype assumes bfloat", "", "none", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := BytesPerParam(tc.dtype, tc.quantization); got != tc.want {
				t.Fatalf("BytesPerParam(%q, %q) = %v, want %v", tc.dtype, tc.quantization, got, tc.want)
			}
		})
	}
}

// The wizard cannot ask which runtime to use, so this has to be right for every
// model anyone picks — and for GGUF "right" is vLLM, because the alternative was
// an ollama container that pulled nothing, reported healthy and 404'd every
// completion. A GGUF repository is refused at the model step; it is never routed
// to a runtime that cannot serve it.
func TestEveryPickedModelIsServedByTheRuntimeThatCanActuallyStart(t *testing.T) {
	for _, tc := range []struct {
		name         string
		library      string
		quantization string
	}{
		{"safetensors transformers repo", "transformers", "none"},
		{"quantized transformers repo", "transformers", "awq"},
		{"gguf by library", "gguf", "none"},
		{"gguf by quantization", "", "gguf"},
		{"nothing known at all", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := EngineForModel(tc.library, tc.quantization); got != "vllm" {
				t.Fatalf("EngineForModel(%q, %q) = %q, want vllm", tc.library, tc.quantization, got)
			}
		})
	}
}

// The GGUF card still has to SAY it is GGUF. The engine no longer distinguishes
// it, so the format on the card is the only thing the wizard can refuse it by.
func TestAGGUFRepositoryKeepsTheFormatThatGetsItRefused(t *testing.T) {
	card := ModelCard{ID: "TheBloke/phi-2-GGUF", Library: "gguf"}
	applySizing(&card, []string{"gguf", "text-generation"}, "", 0)

	if card.Quantization != "gguf" {
		t.Fatalf("quantization = %q, want gguf — the wizard has nothing else to recognise the pick by", card.Quantization)
	}
	if card.Engine != "vllm" {
		t.Errorf("engine = %q, want vllm — ollama is no longer derived, it served nothing", card.Engine)
	}
}

func TestFormatVRAMIsTheOnlyPlaceTheSizeStringIsWritten(t *testing.T) {
	for _, tc := range []struct {
		name  string
		bytes uint64
		want  string
	}{
		{"llama 8b", 21_414_029_995, "~21.4 GB"},
		{"a tenth of a gigabyte is the resolution", 5_600_000_000, "~5.6 GB"},
		{"just over a gigabyte", 1_200_000_000, "~1.2 GB"},
		{"below a gigabyte falls back to megabytes", 512_000_000, "~512 MB"},
		{"a tiny model still has a size", 900_000, "~1 MB"},
		// A gigabyte spelled the long way. The unit has to be chosen from the
		// rounded figure, not from the byte count that produced it.
		{"a hair under a gigabyte is a gigabyte", 999_999_999, "~1.0 GB"},
		// Past 100 GB a tenth is noise, and rounding UP is what keeps the figure
		// from landing on a truncated capacity.
		{"past a hundred gigabytes it rounds up to whole ones", 187_733_333_333, "~188 GB"},
		{"an exact hundred and one", 101_000_000_000, "~101 GB"},
		// Not "~0 GB": an unsized model has no size to show, and a UI can test
		// for empty where it cannot test for a number that lies.
		{"unsized renders as nothing", 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := FormatVRAM(tc.bytes); got != tc.want {
				t.Fatalf("FormatVRAM(%d) = %q, want %q", tc.bytes, got, tc.want)
			}
		})
	}
}

// A refusal states both numbers in one sentence, and the two are rendered by
// different code: the requirement by FormatVRAM here, the card's capacity by
// store.humanBytes (and the web's formatReportedBytes, which is the same four
// lines in TypeScript). Those truncate. Rounding to whole gigabytes here meant a
// 17.33 GB model on a 17.18 GB card refused with "needs ~17 GB but this server
// has 17 GB" — a sentence with no information in it and no way for the operator
// to tell it from a bug.
func TestARefusalCannotQuoteTheSameNumberTwice(t *testing.T) {
	// The capacity renderer, transcribed rather than imported: this package must
	// not depend on the store (see the package comment), and the assertion is
	// about the pair of strings a human reads, not about either function.
	truncatedGB := func(bytes uint64) string {
		return fmt.Sprintf("%d GB", bytes/1_000_000_000)
	}

	for _, tc := range []struct {
		name     string
		required uint64
		capacity uint64
	}{
		{"the pair from the review", 17_330_000_000, 17_180_000_000},
		{"a whole gigabyte apart", 25_000_000_000, 24_000_000_000},
		{"barely over", 24_000_000_001, 24_000_000_000},
		// Above the decimal cut-off the requirement rounds up instead, which
		// buys the same guarantee on a card nobody had when this was written.
		{"two big cards", 141_400_000_000, 141_000_000_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			need, have := FormatVRAM(tc.required), truncatedGB(tc.capacity)
			if strings.TrimPrefix(need, "~") == have {
				t.Fatalf("a refusal would read %q needed against %q available — the operator learns nothing", need, have)
			}
		})
	}
}

// AWQ and GPTQ pack eight weights into one tensor element, so the Hub's element
// count is not a parameter count. Trusting it would claim a 7B model fits in
// under a gigabyte and wave through a deploy that cannot start.
func TestAQuantizedRepositoryIsSizedFromItsNameNotItsPackedTensorCount(t *testing.T) {
	card := ModelCard{ID: "TheBloke/Llama-2-7B-Chat-AWQ", Library: "transformers"}
	applySizing(&card, []string{"safetensors", "awq", "4-bit"}, "F16", 1_130_000_000)

	if card.Quantization != "awq" {
		t.Fatalf("quantization = %q, want awq", card.Quantization)
	}
	if card.BytesPerParam != 0.5 {
		t.Errorf("bytesPerParam = %v, want half of the F16 the repo also reports", card.BytesPerParam)
	}
	if card.SizingBasis != "name" || card.Parameters != 7_000_000_000 {
		t.Errorf("sizing = %s/%d, want name/7e9 — the packed element count is not a parameter count", card.SizingBasis, card.Parameters)
	}
	// 7e9 × 0.5 = 3.5 GB of weights, × 1.2 ÷ 0.9 ≈ 4.7 GB.
	if card.VRAMText != "~4.7 GB" {
		t.Errorf("vramText = %q, want ~4.7 GB", card.VRAMText)
	}
	if card.Engine != "vllm" {
		t.Errorf("engine = %q, want vllm — AWQ is a vLLM format", card.Engine)
	}
}

// safetensors.total is a third party's integer and it is not always a parameter
// count. An absurd one used to be multiplied out into a byte figure that
// overflowed and printed as "~9223372037 GB" — a fit check gating a real deploy
// on a number with no meaning. The name is the fallback, exactly as it is for a
// repository with no index at all.
func TestACorruptSafetensorsTotalIsRefusedRatherThanSized(t *testing.T) {
	card := ModelCard{ID: "some-lab/Mistral-7B-Instruct", Library: "transformers"}
	applySizing(&card, []string{"transformers"}, "BF16", 1<<63)

	if card.SizingBasis != "name" || card.Parameters != 7_000_000_000 {
		t.Fatalf("sizing = %s/%d, want name/7e9 — an impossible index must fall through to the name",
			card.SizingBasis, card.Parameters)
	}
	if card.VRAMText != "~18.7 GB" {
		t.Errorf("vramText = %q, want the name-derived size", card.VRAMText)
	}

	// A repository with an impossible index AND no size in its name is simply
	// unsized, which is the same answer as any other model nobody can size.
	nameless := ModelCard{ID: "some-lab/experimental", Library: "transformers"}
	applySizing(&nameless, nil, "BF16", 1<<63)
	if nameless.ParametersKnown || nameless.VRAMBytesRequired != 0 {
		t.Errorf("card = %+v, want no size at all rather than an invented one", nameless)
	}
}

// The rule that keeps the product honest: a size we had to guess at is no size
// at all, and nothing downstream gets a number to gate on.
func TestAModelNobodyCanSizeCarriesNoFitCheck(t *testing.T) {
	card := ModelCard{ID: "some-lab/experimental-instruct", Library: "transformers"}
	applySizing(&card, []string{"transformers"}, "", 0)

	if card.ParametersKnown {
		t.Fatal("parametersKnown = true for a model with neither an index nor a size in its name")
	}
	if card.Parameters != 0 {
		t.Errorf("parameters = %d, want 0", card.Parameters)
	}
	if card.VRAMBytesRequired != 0 {
		t.Errorf("vramBytesRequired = %d, want 0 — zero is what switches the fit check off", card.VRAMBytesRequired)
	}
	if card.VRAMText != "" {
		t.Errorf("vramText = %q, want empty", card.VRAMText)
	}
	if card.SizingBasis != "unknown" {
		t.Errorf("sizingBasis = %q, want unknown", card.SizingBasis)
	}
	// It is still deployable: the engine and the id are all a deploy needs.
	if card.Engine != "vllm" {
		t.Errorf("engine = %q — an unsizable model must still be runnable", card.Engine)
	}
}

func TestRequiredVRAMBytesRefusesToInventANumber(t *testing.T) {
	for _, tc := range []struct {
		name          string
		parameters    uint64
		bytesPerParam float64
		want          uint64
	}{
		{"no parameters means no estimate", 0, 2, 0},
		{"no bytes per parameter means no estimate", 8_000_000_000, 0, 0},
		{"a nonsense width means no estimate", 8_000_000_000, -2, 0},
		{"seven billion at half a byte", 7_000_000_000, 0.5, 4_666_666_667},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RequiredVRAMBytes(tc.parameters, tc.bytesPerParam); got != tc.want {
				t.Fatalf("RequiredVRAMBytes(%d, %v) = %d, want %d", tc.parameters, tc.bytesPerParam, got, tc.want)
			}
		})
	}
}

// The safetensors index lists position ids and attention masks beside the
// weights. They say nothing about how many bytes a parameter costs.
func TestTheDominantDTypeIgnoresIndexAndMaskTensors(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   map[string]uint64
		want string
	}{
		{"a bfloat model", map[string]uint64{"BF16": 8_030_261_248, "I64": 8192}, "BF16"},
		{"integer tensors outnumber the weights", map[string]uint64{"F16": 100, "I64": 999_999, "BOOL": 500}, "F16"},
		{"an fp8 checkpoint", map[string]uint64{"F8_E4M3": 7_000_000_000, "BF16": 500_000}, "F8_E4M3"},
		{"a mixed-precision checkpoint takes the majority", map[string]uint64{"F32": 10, "BF16": 8_000_000_000}, "BF16"},
		{"nothing float at all", map[string]uint64{"I64": 10}, ""},
		{"no index", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := dominantDType(tc.in); got != tc.want {
				t.Fatalf("dominantDType(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestQuantizationIsReadOffTheRepositoryIdAndTags(t *testing.T) {
	for _, tc := range []struct {
		name   string
		repoID string
		tags   []string
		dtype  string
		want   string
	}{
		{"an ordinary safetensors repo", "meta-llama/Llama-3.1-8B-Instruct", []string{"transformers"}, "BF16", "none"},
		{"gguf in the name", "TheBloke/Llama-2-7B-GGUF", nil, "", "gguf"},
		{"gguf in the tags", "someone/llama-conversion", []string{"gguf"}, "", "gguf"},
		{"awq", "TheBloke/Llama-2-7B-AWQ", nil, "F16", "awq"},
		{"gptq", "TheBloke/Llama-2-7B-GPTQ", nil, "F16", "gptq"},
		{"fp8 from the dtype alone", "neuralmagic/Llama-3.1-8B-quantized", nil, "F8_E4M3", "fp8"},
		// bitsandbytes names no vendor. "awq" is the closest of the five values
		// the wire contract defines and sizes identically; the number is what
		// gates the deploy, the label only names it.
		{"a four-bit repo that names no vendor", "unsloth/Llama-3.2-3B-Instruct-bnb-4bit", nil, "", "awq"},
		{"gguf wins over a four-bit tag", "TheBloke/Llama-2-7B-GGUF", []string{"4-bit"}, "", "gguf"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := detectQuantization(tc.repoID, tc.tags, tc.dtype); got != tc.want {
				t.Fatalf("detectQuantization(%q, %v, %q) = %q, want %q", tc.repoID, tc.tags, tc.dtype, got, tc.want)
			}
		})
	}
}
