package reconciler

// What an inference endpoint's container op declares, and specifically what it
// does NOT declare.
//
// A secret reference the control plane cannot answer is FATAL on the agent —
// the container driver returns "secret %q referenced but not provided by the
// control plane" and the apply fails — so which references this renders is not
// a detail of the document, it is whether the endpoint starts at all. The
// runtime catalog used to name HUGGING_FACE_HUB_TOKEN on every vLLM endpoint
// while nothing in the product ever created that secret, which made the most
// ordinary deploy there is (a public model on a control plane holding no Hub
// token) a container that never ran.

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/hf"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

const llmMeshIP = "10.77.0.9"

// renderOneLLM renders a single vLLM endpoint and returns its container spec.
func renderOneLLM(t *testing.T, target store.LLMTarget, refs []store.SecretRefMeta) containerOpSpec {
	t.Helper()
	specs := []store.ResourceSpec{{
		ResourceID: "res_llm", ProjectID: "proj_x", Kind: "llm",
		Spec: json.RawMessage(`{"engine":"vllm","model":"meta-llama/Llama-3.1-8B"}`),
	}}
	ops, _ := renderOps("srv_gpu", specs, nil,
		map[string][]store.SecretRefMeta{"res_llm": refs},
		store.HostHardening{MeshIP: llmMeshIP}, nil, nil, nil, nil,
		map[string]store.LLMTarget{"res_llm": target},
		nil, nil, ACMEConfig{}, clusterRender{}, registryRender{})

	op, ok := opByID(ops, "res:res_llm")
	if !ok {
		t.Fatalf("no container op rendered for the endpoint: %+v", ops)
	}
	if op.Kind != dsd.KindContainerApply {
		t.Fatalf("op kind = %q, want %q", op.Kind, dsd.KindContainerApply)
	}
	var cs containerOpSpec
	if err := json.Unmarshal(op.Spec, &cs); err != nil {
		t.Fatal(err)
	}
	return cs
}

func secretRefNames(cs containerOpSpec) []string {
	out := make([]string, 0, len(cs.SecretRefs))
	for _, r := range cs.SecretRefs {
		out = append(out, r.Name)
	}
	return out
}

func TestAnEndpointReferencesItsHuggingFaceTokenOnlyWhenOneExists(t *testing.T) {
	seeded := renderOneLLM(t, store.LLMTarget{Port: 21000, WeightsToken: true}, nil)
	if names := secretRefNames(seeded); len(names) != 1 || names[0] != store.HubTokenSecretName {
		t.Fatalf("secret refs = %v, want just %s", names, store.HubTokenSecretName)
	}
	if !seeded.SecretRefs[0].EnvVar {
		t.Error("the runtime reads its token from the environment, not from a file")
	}

	// The one that used to break every public-model deploy: no credential
	// stored, so no reference — otherwise the agent refuses the container over a
	// token the model did not need.
	bare := renderOneLLM(t, store.LLMTarget{Port: 21000}, nil)
	if names := secretRefNames(bare); len(names) != 0 {
		t.Fatalf("secret refs = %v on an endpoint with no credential; the agent fails the apply "+
			"on a reference the control plane will not answer", names)
	}
}

// The tenant's own HUGGING_FACE_HUB_TOKEN and the seeded one are one
// environment variable, and the document must ask for it once. Two references
// to a single name is an argument about which value wins, decided by whichever
// loop the agent happens to run last.
func TestTheTokenIsReferencedOnceEvenWhenTheTenantSuppliesItToo(t *testing.T) {
	cs := renderOneLLM(t, store.LLMTarget{Port: 21000, WeightsToken: true},
		[]store.SecretRefMeta{{ResourceID: "res_llm", Name: store.HubTokenSecretName, EnvVar: true}})

	seen := 0
	for _, name := range secretRefNames(cs) {
		if name == store.HubTokenSecretName {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("%s referenced %d times: %v", store.HubTokenSecretName, seen, secretRefNames(cs))
	}
}

// The endpoint's other secrets are untouched by any of this: an API key the
// model server presents still rides along.
func TestAnEndpointStillCarriesItsOwnSecrets(t *testing.T) {
	cs := renderOneLLM(t, store.LLMTarget{Port: 21000},
		[]store.SecretRefMeta{{ResourceID: "res_llm", Name: "SERVED_API_KEY", EnvVar: true}})
	names := secretRefNames(cs)
	if len(names) != 1 || names[0] != "SERVED_API_KEY" {
		t.Fatalf("secret refs = %v, want the resource's own secret", names)
	}
}

// maxModelLen reads the rendered --max-model-len, or "" when the flag is absent.
func maxModelLen(cs containerOpSpec) string {
	for i, arg := range cs.Command {
		if arg == "--max-model-len" && i+1 < len(cs.Command) {
			return cs.Command[i+1]
		}
	}
	return ""
}

// The window the endpoint was provisioned for is the window it is started at —
// the document carries the ENDPOINT's number, not a package constant.
//
// Both ends of that range are fatal, which is why this cannot be a literal.
// Unpinned, vLLM sizes its KV cache from the model's own
// max_position_embeddings, many times what hf.SizedContextTokens budgets, and
// the runtime exits demanding memory the fit check never estimated. Pinned to
// hf.SizedContextTokens for every model — which is what this used to render —
// vLLM refuses to start anything whose own ceiling is shorter, so TinyLlama at
// 2048 crash-looped on a card it fits many times over.
func TestTheStartCommandPinsTheWindowTheEndpointWasProvisionedFor(t *testing.T) {
	sized := renderOneLLM(t, store.LLMTarget{Port: 21000, ContextTokens: hf.SizedContextTokens}, nil)
	if got, want := maxModelLen(sized), strconv.Itoa(hf.SizedContextTokens); got != want {
		t.Fatalf("--max-model-len = %q, want the endpoint's own %q; a runtime allowed to size its "+
			"KV cache from the model's limit exits on a card the fit check approved", got, want)
	}

	// A model whose own ceiling is shorter than the sizing window is served at
	// ITS ceiling. 2048 is TinyLlama-1.1B-Chat, the catalogue's "fits anything"
	// pick and therefore the model this failure hit most often.
	short := renderOneLLM(t, store.LLMTarget{Port: 21000, ContextTokens: 2048}, nil)
	if got := maxModelLen(short); got != "2048" {
		t.Fatalf("--max-model-len = %q for a 2048-token model, want 2048; vLLM raises rather than "+
			"clamping when the flag exceeds the model's own maximum", got)
	}
}

// A zero on the endpoint row renders no flag, and that case is now only
// reachable for rows written before any of this existed.
//
// It is worth stating what changed, because omitting used to be the DESIGN and
// it was wrong. hf.ServedContextTokens no longer returns zero: an unreadable
// ceiling falls back to the window the VRAM estimate budgeted, because the
// models whose ceiling cannot be read are the GATED ones — Llama and its kin,
// whose real ceilings are far above that window — and leaving those unpinned is
// the 131072-token KV-cache crash the flag exists to prevent, with a green fit
// check in front of it. Migration 0056 backfills existing rows for the same
// reason, so this guard should never fire in production.
//
// It stays anyway. A row that says zero is a row that says nothing, and
// rendering "--max-model-len 0" against it would be a container that cannot
// start for a reason nobody could read off the document.
func TestAnEndpointRowWithNoWindowRendersNoFlagRatherThanAZeroOne(t *testing.T) {
	cs := renderOneLLM(t, store.LLMTarget{Port: 21000}, nil)
	if got := maxModelLen(cs); got != "" {
		t.Fatalf("--max-model-len = %q for an endpoint row carrying no window, want the flag omitted", got)
	}
	// The rest of the command is untouched — this omits one flag, it does not
	// stop serving the model.
	if len(cs.Command) == 0 || cs.Command[0] != "--model" {
		t.Fatalf("command = %v, want the model still passed to the runtime", cs.Command)
	}
}
