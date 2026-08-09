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

// The context window the fit check paid for has to reach the runtime, or the
// estimate approved a deploy that cannot start: vLLM otherwise sizes its KV
// cache from the model's own max_position_embeddings, which is many times the
// window hf.SizedContextTokens budgets for.
func TestTheStartCommandPinsTheContextTheFitCheckBudgeted(t *testing.T) {
	cs := renderOneLLM(t, store.LLMTarget{Port: 21000}, nil)
	got := ""
	for i, arg := range cs.Command {
		if arg == "--max-model-len" && i+1 < len(cs.Command) {
			got = cs.Command[i+1]
		}
	}
	// Compared against the sizing package's own constant, not a literal: the
	// number here and the number the VRAM estimate budgets are the same number,
	// and a test that spelled it out again would keep passing while they drifted.
	if want := strconv.Itoa(hf.SizedContextTokens); got != want {
		t.Fatalf("--max-model-len = %q, want the sized context %q; a runtime allowed to size its "+
			"KV cache from the model's own limit exits on a card the fit check approved", got, want)
	}
}
