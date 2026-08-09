package store

// The create-time model checks have exactly two jobs, and the tests below are
// split along them: refuse a model this control plane provably cannot serve or
// cannot fit, and refuse NOTHING the moment the fact behind the refusal stops
// being provable. The second job is the one worth testing hardest — every
// fail-open path here is a huggingface.co incident that would otherwise stop an
// entire fleet from deploying model endpoints on hardware it already owns.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/hf"
)

// L40S / A10G / H100 figures as an agent reports them, so the arithmetic in
// these tests is the arithmetic a real host produces.
const (
	vram24GB  = 23_609_344_000  // NVIDIA A10G
	vram48GB  = 48_301_604_864  // NVIDIA L40S
	vram80GB  = 85_520_809_984  // NVIDIA H100 80GB
	llama8B   = 21_414_029_995  // 8.03B params × 2 bytes × 1.2 ÷ 0.9
	llama70B  = 187_904_819_200 // 70B at bf16 — fits nothing below an H100 cluster
	llama70B4 = 46_976_204_800  // the same model as a 4-bit AWQ build
)

// sized is a model the control plane could size, with its sentence RENDERED
// rather than typed.
//
// Typing it is how these fixtures went stale: they said "~21 GB" while
// FormatVRAM emits "~21.4 GB", so every one of them described a refusal this
// control plane cannot produce — and the tests stayed green, because nothing
// here had ever called the formatter. The refusal quotes VRAMText verbatim, so
// the fixture has to come from the same function the picker's card does.
func sized(bytes uint64) ModelSize {
	return ModelSize{ParametersKnown: true, VRAMBytesRequired: bytes, VRAMText: hf.FormatVRAM(bytes)}
}

// fakeSizer stands in for the Hub. It records what it was asked so a test can
// prove the store did NOT ask — "we never dialled huggingface.co for an Ollama
// tag" is a behaviour, not an implementation detail: it is the difference
// between one round trip per create and a guaranteed 404 per create.
type fakeSizer struct {
	size  ModelSize
	err   error
	asked []string
}

func (f *fakeSizer) SizeModel(_ context.Context, repoID string) (ModelSize, error) {
	f.asked = append(f.asked, repoID)
	if f.err != nil {
		return ModelSize{}, f.err
	}
	return f.size, nil
}

func TestAModelBiggerThanTheCardIsRefusedWithBothNumbers(t *testing.T) {
	err := checkModelFits("meta-llama/Llama-3.1-70B-Instruct", sized(llama70B), vram24GB, "gpu-hel-01")
	if err == nil {
		t.Fatal("a 188 GB model on a 24 GB card was accepted; the create-time re-check is not running")
	}
	var inv ErrInvalid
	if !errors.As(err, &inv) {
		t.Fatalf("refusal is %T, want ErrInvalid so the API answers 422 rather than 500", err)
	}
	// Both numbers, or the operator cannot tell whether they need a different
	// model or a different machine.
	for _, want := range []string{"meta-llama/Llama-3.1-70B-Instruct", "~188 GB", "gpu-hel-01", "23 GB"} {
		if !strings.Contains(inv.Msg, want) {
			t.Errorf("refusal does not mention %q: %s", want, inv.Msg)
		}
	}
	// And the way out. A refusal that names no remedy sends the operator to
	// support to be told about 4-bit builds.
	if !strings.Contains(inv.Msg, "quantized") {
		t.Errorf("refusal names no remedy: %s", inv.Msg)
	}
}

func TestTheFitCheckAcceptsWhatActuallyFits(t *testing.T) {
	for _, tc := range []struct {
		name    string
		size    ModelSize
		perGPU  uint64
		wantErr bool
	}{
		{"8B on a 24 GB card", sized(llama8B), vram24GB, false},
		{"8B on an 80 GB card", sized(llama8B), vram80GB, false},
		{"70B on a 48 GB card", sized(llama70B), vram48GB, true},
		// The quantized build of the model above is the remedy the refusal
		// suggests, so it had better be accepted where the bf16 one is not.
		{"the same 70B as 4-bit AWQ on a 48 GB card", sized(llama70B4), vram48GB, false},
		// Exactly at the line is a fit: the 20% KV/activation margin and the 90%
		// utilization cap are already inside the required figure, so subtracting
		// a second safety margin here would refuse models that run.
		{"a model sized exactly to the card", sized(vram24GB), vram24GB, false},
		{"one byte over the card", sized(vram24GB + 1), vram24GB, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkModelFits("some/model", tc.size, tc.perGPU, "gpu-1")
			if tc.wantErr && err == nil {
				t.Fatalf("%d bytes on a %d byte card was accepted", tc.size.VRAMBytesRequired, tc.perGPU)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("%d bytes on a %d byte card was refused: %v", tc.size.VRAMBytesRequired, tc.perGPU, err)
			}
		})
	}
}

// The fail-open contract, stated as a table because each row is a real outage
// this check must not cause.
func TestTheFitCheckRefusesNothingItCannotProve(t *testing.T) {
	for _, tc := range []struct {
		name   string
		size   ModelSize
		perGPU uint64
	}{
		// The Hub knew the repo but neither its safetensors index nor its name
		// gave a parameter count. Sizing it would be a guess, and a guess must
		// never cost someone a deploy.
		{"the model could not be sized", ModelSize{ParametersKnown: false}, vram24GB},
		// A sizer that reports "known" with no bytes is incoherent; treat it as
		// unknown rather than as a zero-byte model that fits everything, and
		// certainly not as grounds to refuse.
		{"a known-but-empty size", ModelSize{ParametersKnown: true, VRAMBytesRequired: 0}, vram24GB},
		// An agent older than SIGMA-201, or one whose nvidia probe failed this
		// tick. Absent is unknown, not zero — the same rule the registration gate
		// holds.
		{"the host reported no GPU", ModelSize{ParametersKnown: true, VRAMBytesRequired: llama70B}, 0},
		{"neither number is known", ModelSize{}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkModelFits("meta-llama/Llama-3.1-70B-Instruct", tc.size, tc.perGPU, "gpu-1"); err != nil {
				t.Fatalf("create was refused on an unknown: %v", err)
			}
		})
	}
}

// The other half of fail-open, one level up: the sizer itself. A Hub that is
// down, slow or confused must produce the same ModelSize as no sizer at all.
func TestAnUnreachableHubSizesAsUnknownRatherThanFailingTheCreate(t *testing.T) {
	spec := json.RawMessage(`{"engine":"vllm","model":"meta-llama/Llama-3.1-70B-Instruct"}`)

	sizer := &fakeSizer{err: errors.New("dial tcp huggingface.co:443: i/o timeout")}
	st := &Store{modelSizer: sizer}
	size := st.sizeModelForFit(context.Background(), spec)
	if size.ParametersKnown {
		t.Fatalf("a Hub timeout produced a usable size %+v — the create would be refused during an HF incident", size)
	}
	if len(sizer.asked) != 1 {
		t.Fatalf("sizer was asked %v, want exactly the requested repo", sizer.asked)
	}
	// And with no sizer configured at all (a self-hoster who wired none, and
	// every test in this package) the outcome is identical: no check.
	if size := (&Store{}).sizeModelForFit(context.Background(), spec); size.ParametersKnown {
		t.Fatalf("an unconfigured store produced a size %+v", size)
	}

	// The success path still has to work, or the two fail-open branches above
	// would pass on a check that never fires.
	sizer = &fakeSizer{size: sized(llama70B)}
	got := (&Store{modelSizer: sizer}).sizeModelForFit(context.Background(), spec)
	if !got.ParametersKnown || got.VRAMBytesRequired != llama70B || got.VRAMText != hf.FormatVRAM(llama70B) {
		t.Fatalf("size = %+v, want the sizer's answer passed through verbatim", got)
	}
}

// An `llm` resource can run on Ollama, whose model references are library tags,
// not Hub repo ids. Asking the Hub about `llama3.2:3b` earns a 404 every single
// time — harmless, since a 404 fails open, but a round trip spent to learn
// nothing on every Ollama create.
func TestOnlyAHubRepoIdIsEverSizedAgainstTheHub(t *testing.T) {
	for _, tc := range []struct {
		name    string
		spec    string
		wantAsk string // "" ⇒ the Hub must not be contacted at all
	}{
		{"a vllm repo id", `{"engine":"vllm","model":"meta-llama/Llama-3.1-8B-Instruct"}`, "meta-llama/Llama-3.1-8B-Instruct"},
		{"a repo id with surrounding space", `{"model":"  Qwen/Qwen2.5-7B-Instruct "}`, "Qwen/Qwen2.5-7B-Instruct"},
		{"an ollama tag", `{"engine":"ollama","model":"llama3.2:3b"}`, ""},
		{"a bare ollama name", `{"engine":"ollama","model":"mistral"}`, ""},
		{"an ollama namespaced tag", `{"engine":"ollama","model":"library/llama3:70b"}`, ""},
		{"a path inside a repo", `{"model":"TheBloke/Llama-2-7B-GGUF/model.gguf"}`, ""},
		{"no model at all", `{"engine":"vllm"}`, ""},
		{"a spec that is not JSON", `not json`, ""},
		{"an empty spec", ``, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sizer := &fakeSizer{size: sized(llama8B)}
			st := &Store{modelSizer: sizer}
			size := st.sizeModelForFit(context.Background(), json.RawMessage(tc.spec))
			if tc.wantAsk == "" {
				if len(sizer.asked) != 0 {
					t.Fatalf("the Hub was asked about %v, which is not a repo id", sizer.asked)
				}
				if size.ParametersKnown {
					t.Fatalf("size = %+v, want unknown so nothing is refused", size)
				}
				return
			}
			if len(sizer.asked) != 1 || sizer.asked[0] != tc.wantAsk {
				t.Fatalf("the Hub was asked %v, want [%s]", sizer.asked, tc.wantAsk)
			}
		})
	}
}

// A repository vLLM cannot open is refused here and not only in the wizard.
// This is the one refusal whose absence is INVISIBLE: the container starts,
// reports healthy, and answers 404 to every completion on a GPU-billed host, so
// nothing in the product notices until somebody tries to use the endpoint.
func TestAModelNoRuntimeHereCanServeIsRefusedWithSomethingToPickInstead(t *testing.T) {
	err := checkModelServable("TheBloke/phi-2-GGUF", ModelSize{Quantization: "gguf"})
	var inv ErrInvalid
	if !errors.As(err, &inv) {
		t.Fatalf("a GGUF repository err = %v, want ErrInvalid — an API-direct create of one is "+
			"accepted today and serves nothing", err)
	}
	// The remedy is the point: "GGUF is unsupported" leaves an operator who
	// picked it to save VRAM with no idea that AWQ exists.
	for _, want := range []string{"TheBloke/phi-2-GGUF", "safetensors", "AWQ"} {
		if !strings.Contains(inv.Msg, want) {
			t.Errorf("refusal does not mention %q: %s", want, inv.Msg)
		}
	}

	err = checkModelServable("sentence-transformers/all-MiniLM-L6-v2",
		ModelSize{PipelineTag: "sentence-similarity"})
	if !errors.As(err, &inv) {
		t.Fatalf("an embedding model err = %v, want ErrInvalid", err)
	}
	for _, want := range []string{"sentence-similarity", hf.TextGenerationTask} {
		if !strings.Contains(inv.Msg, want) {
			t.Errorf("refusal does not name the task it got or the task it wants: %s", inv.Msg)
		}
	}
}

// The same fail-open contract the fit check holds, on the same input: a model
// nobody could look up carries no format and no task, and neither absence is a
// reason to refuse.
func TestAModelTheHubWouldNotDescribeIsStillDeployable(t *testing.T) {
	for _, tc := range []struct {
		name string
		size ModelSize
	}{
		// Hub down, Hub slow, no sizer wired, an Ollama tag that was never looked
		// up: every one of them is the zero ModelSize.
		{"nothing came back from the Hub at all", ModelSize{}},
		// The case that makes empty mean UNKNOWN rather than "not a text model":
		// a gated repository read without a token carries no metadata, and
		// refusing on it would block every gated repo on a tokenless control
		// plane — the one operator who can do least about it.
		{"a gated repo resolved without a token has no task", ModelSize{Quantization: "none"}},
		{"a quantization we serve fine", sized(llama70B4)},
		{"the task the runtime is for", ModelSize{PipelineTag: hf.TextGenerationTask, Quantization: "awq"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkModelServable("some/model", tc.size); err != nil {
				t.Fatalf("create was refused on an unknown: %v", err)
			}
		})
	}
}

// The provisioning path and the fit check must read the same model out of the
// same spec. Two decoders is how the check ends up refusing a model the runtime
// was never going to load — the worst possible version of this feature.
func TestOneDecoderReadsTheLLMSpec(t *testing.T) {
	spec := json.RawMessage(`{"engine":"vllm","model":"Qwen/Qwen2.5-7B-Instruct","unrelated":true}`)
	got := parseLLMSpec(spec)
	if got.Engine != "vllm" || got.Model != "Qwen/Qwen2.5-7B-Instruct" {
		t.Fatalf("parseLLMSpec = %+v", got)
	}
	// A spec the caller never sent, or sent badly, means "the caller said
	// nothing" — the insert path then applies the default engine, exactly as it
	// did before this decoder was shared.
	for _, bad := range []string{``, `null`, `"model"`, `{"engine":`} {
		if got := parseLLMSpec(json.RawMessage(bad)); got.Engine != "" || got.Model != "" {
			t.Fatalf("parseLLMSpec(%q) = %+v, want the zero value", bad, got)
		}
	}
}
