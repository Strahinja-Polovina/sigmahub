package hf

import "testing"

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
	if got := FormatVRAM(required); got != "~21 GB" {
		t.Fatalf("FormatVRAM = %q, want ~21 GB", got)
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

// The wizard used to ask which runtime to use. It cannot ask any more, so this
// has to be right for every model anyone picks.
func TestTheModelsOwnMetadataChoosesTheRuntime(t *testing.T) {
	for _, tc := range []struct {
		name         string
		library      string
		quantization string
		want         string
	}{
		{"safetensors transformers repo", "transformers", "none", "vllm"},
		{"quantized transformers repo", "transformers", "awq", "vllm"},
		{"gguf by library", "gguf", "none", "ollama"},
		{"gguf by quantization", "", "gguf", "ollama"},
		{"nothing known at all", "", "", "vllm"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := EngineForModel(tc.library, tc.quantization); got != tc.want {
				t.Fatalf("EngineForModel(%q, %q) = %q, want %q", tc.library, tc.quantization, got, tc.want)
			}
		})
	}
}

func TestFormatVRAMIsTheOnlyPlaceTheSizeStringIsWritten(t *testing.T) {
	for _, tc := range []struct {
		name  string
		bytes uint64
		want  string
	}{
		{"llama 8b", 21_414_029_995, "~21 GB"},
		{"rounds to the nearest gigabyte", 5_600_000_000, "~6 GB"},
		{"just over a gigabyte", 1_200_000_000, "~1 GB"},
		{"below a gigabyte falls back to megabytes", 512_000_000, "~512 MB"},
		{"a tiny model still has a size", 900_000, "~1 MB"},
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
	if card.VRAMText != "~5 GB" {
		t.Errorf("vramText = %q, want ~5 GB", card.VRAMText)
	}
	if card.Engine != "vllm" {
		t.Errorf("engine = %q, want vllm — AWQ is a vLLM format", card.Engine)
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
