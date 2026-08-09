package integration

// The create-time VRAM fit check and the weights credential, driven through
// CreateResource against a real database (SIGMA-213, SIGMA-214).
//
// The unit tests in cp/internal/store call checkModelFits, maxVRAMPerGPU and
// sizeModelForFit directly. All three stayed green when the checkModelFits call
// was deleted from domain.go, which is to say they proved the arithmetic and
// nothing about the feature: the rule only exists where CreateResource applies
// it, and CreateResource applies it in THREE places — the server branch, the
// cluster branch, and every fail-open path through both. So these drive the
// real store, with the real SQL that reads a host's facts and a cluster's
// membership, and each one fails if its call site goes away.
//
// The fail-open cases are the ones worth the harness. A refusal is one
// comparison; "refuse NOTHING the moment either number stops being provable" is
// a query returning no rows, a JSONB column with no gpu key, and a sizer that
// answered "I don't know" — none of which a hand-built ModelSize can stand in
// for.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// Real card figures, so the sentences these tests assert on are the sentences
// an operator reads.
const (
	itVRAM24GB = 23_609_344_000  // NVIDIA A10G
	itVRAM48GB = 48_301_604_864  // NVIDIA L40S
	itLlama70B = 187_904_819_200 // 70B at bf16
	itLlama70Q = 46_976_204_800  // the same model as a 4-bit AWQ build
)

// stubSizer is the Hub's answer, fixed. The integration harness must make no
// network call, and what these tests are about is what CreateResource DOES with
// a size, not how the size was obtained.
type stubSizer struct{ size store.ModelSize }

func (s stubSizer) SizeModel(context.Context, string) (store.ModelSize, error) { return s.size, nil }

// knownSize is a model the control plane could size; unknownSize is every way
// it could not (Hub down, no safetensors index, an Ollama tag, no sizer).
func knownSize(bytes uint64, text string) store.ModelSize {
	return store.ModelSize{ParametersKnown: true, VRAMBytesRequired: bytes, VRAMText: text}
}

// gpuHost enrolls a GPU server and reports a card of the given size on it. A
// perGPU of 0 reports facts with NO gpu key at all, which is the agent that
// predates the inventory or whose nvidia probe failed this tick — the
// difference between "no GPU" and "we were not told", and the whole reason the
// check is allowed to skip.
func gpuHost(t *testing.T, st *store.Store, orgID, name string, perGPU uint64) string {
	t.Helper()
	id := connectTypedServer(t, st, orgID, name, "gpu")
	facts := json.RawMessage(`{"arch":"amd64","distro":"ubuntu-24.04"}`)
	if perGPU > 0 {
		// driverVersion is not decoration: the SIGMA-203 gate marks a `gpu` host
		// whose card enumerates without a driver INCOMPATIBLE, and CreateResource
		// refuses an incompatible host before it ever reaches the fit check — so
		// a fixture that omitted it would assert the wrong refusal.
		facts = json.RawMessage(fmt.Sprintf(
			`{"arch":"amd64","distro":"ubuntu-24.04","gpu":{"vendor":"nvidia","count":1,`+
				`"vramBytesPerGpu":%d,"driverVersion":"550.90.07"}}`, perGPU))
	}
	if err := st.RecordHeartbeat(context.Background(), id, store.HeartbeatInput{Facts: facts}); err != nil {
		t.Fatal(err)
	}
	return id
}

// llmFitFixture is a project + environment with one GPU server attached.
func llmFitFixture(t *testing.T, st *store.Store, orgID string, perGPU uint64) (projectID, envID, serverID string) {
	t.Helper()
	ctx := context.Background()
	proj, err := st.CreateProject(ctx, orgID, "ai", "", "admin")
	if err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, orgID, proj.ID, "production", true, "admin")
	if err != nil {
		t.Fatal(err)
	}
	serverID = gpuHost(t, st, orgID, "gpu-hel-01", perGPU)
	if err := st.AttachServer(ctx, orgID, env.ID, serverID, "admin"); err != nil {
		t.Fatal(err)
	}
	return proj.ID, env.ID, serverID
}

// assertNotAFitRefusal is how a cluster-targeted create asserts that the fit
// check let it through, and it is deliberately narrower than "err == nil".
//
// Aiming an `llm` at a CLUSTER cannot currently succeed for a reason that has
// nothing to do with SIGMA-214: provisionLLMTx allocates a mesh port against
// resources.server_id, a cluster workload has none, and the insert dies on
// llm_endpoints_server_id_fkey — a raw SQLSTATE surfaced as a 500. That is a
// separate open defect (the kind is in neither clusterExcludedKinds nor the
// cluster renderer, so it is offered, accepted and then unrunnable), and until
// it is decided one way or the other these tests must still be able to state
// the fit check's own contract: it refused NOTHING. Asserting err == nil here
// would make this file fail for somebody else's bug; asserting nothing at all
// would let the cluster branch quietly start failing closed.
func assertNotAFitRefusal(t *testing.T, err error) {
	t.Helper()
	var invalid store.ErrInvalid
	if errors.As(err, &invalid) && strings.Contains(invalid.Msg, "VRAM") {
		t.Fatalf("the fit check refused a model it could not prove would not fit: %s", invalid.Msg)
	}
}

// createLLM aims a model at a server or a cluster (exactly one of them).
func createLLM(st *store.Store, orgID, envID, serverID, clusterID, name, model string) (store.Resource, error) {
	return st.CreateResource(context.Background(), orgID, store.CreateResourceInput{
		EnvironmentID: envID,
		ServerID:      serverID,
		ClusterID:     clusterID,
		Name:          name,
		Kind:          "llm",
		Spec:          json.RawMessage(`{"engine":"vllm","model":"` + model + `"}`),
	}, "admin")
}

func TestCreatingAModelTooBigForTheServerIsRefusedAtTheAPIBoundary(t *testing.T) {
	st, _ := testStore(t)
	orgID := "org_fit_server"
	_, envID, serverID := llmFitFixture(t, st, orgID, itVRAM24GB)
	st.SetModelSizer(stubSizer{size: knownSize(itLlama70B, "~188 GB")})

	_, err := createLLM(st, orgID, envID, serverID, "", "big", "meta-llama/Llama-3.1-70B-Instruct")
	var invalid store.ErrInvalid
	if !errors.As(err, &invalid) {
		t.Fatalf("creating a 188 GB model on a 24 GB card err = %v, want ErrInvalid (a 422) — "+
			"the create-time re-check is not running on the server branch", err)
	}
	// Both numbers and the model, because a refusal that names one of them
	// leaves the operator guessing whether to change the model or the machine.
	for _, want := range []string{"meta-llama/Llama-3.1-70B-Instruct", "~188 GB", "gpu-hel-01", "23 GB"} {
		if !strings.Contains(invalid.Msg, want) {
			t.Errorf("refusal does not mention %q: %s", want, invalid.Msg)
		}
	}

	// And the remedy it names actually works: the quantized build of the same
	// model fits the same card, so the sentence is advice rather than consolation.
	st.SetModelSizer(stubSizer{size: knownSize(itLlama70Q, "~47 GB")})
	if _, err := createLLM(st, orgID, envID, serverID, "", "quantized",
		"TheBloke/Llama-3.1-70B-AWQ"); err == nil {
		t.Fatal("a 47 GB model was accepted onto a 24 GB card")
	}
	st.SetModelSizer(stubSizer{size: knownSize(itVRAM24GB, "~24 GB")})
	if _, err := createLLM(st, orgID, envID, serverID, "", "exact",
		"some/exactly-sized"); err != nil {
		t.Fatalf("a model sized exactly to the card was refused: %v", err)
	}
}

func TestCreatingAModelTooBigForEveryClusterNodeIsRefused(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_fit_cluster"
	_, envID, controlPlane := llmFitFixture(t, st, orgID, itVRAM24GB)

	// A second, bigger node: the cluster's capacity is its LARGEST card,
	// because Kubernetes only has to place the workload somewhere.
	worker := gpuHost(t, st, orgID, "gpu-hel-02", itVRAM48GB)
	if err := st.AttachServer(ctx, orgID, envID, worker, "admin"); err != nil {
		t.Fatal(err)
	}
	cluster, err := st.CreateCluster(ctx, orgID, store.CreateClusterInput{
		EnvironmentID: envID, Name: "prod", ControlPlaneID: controlPlane,
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddClusterNode(ctx, orgID, cluster.ID, worker, "admin"); err != nil {
		t.Fatal(err)
	}

	st.SetModelSizer(stubSizer{size: knownSize(itLlama70B, "~188 GB")})
	_, err = createLLM(st, orgID, envID, "", cluster.ID, "big", "meta-llama/Llama-3.1-70B-Instruct")
	var invalid store.ErrInvalid
	if !errors.As(err, &invalid) {
		t.Fatalf("creating a 188 GB model against a cluster err = %v, want ErrInvalid — "+
			"without this branch, aiming at the cluster is how you walk around the check", err)
	}
	if !strings.Contains(invalid.Msg, "prod") || !strings.Contains(invalid.Msg, "48 GB") {
		t.Errorf("cluster refusal must name the cluster and its biggest card: %s", invalid.Msg)
	}

	// The same cluster must NOT refuse a model that fits the 48 GB node but not
	// the 24 GB one. Comparing against the smallest card, or the sum, would be a
	// false refusal, and this check is only allowed to be wrong in the permissive
	// direction.
	st.SetModelSizer(stubSizer{size: knownSize(itLlama70Q, "~47 GB")})
	_, err = createLLM(st, orgID, envID, "", cluster.ID, "quantized", "TheBloke/Llama-3.1-70B-AWQ")
	assertNotAFitRefusal(t, err)

	// The wizard needs that number BEFORE it offers the cluster as a target, and
	// it has to be the same number this refusal used — otherwise the dashboard
	// shows the cluster in green and the API refuses after Review.
	list, err := st.ListClusters(ctx, orgID, envID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("clusters = %+v", list)
	}
	if list[0].MaxVRAMBytesPerGPU != itVRAM48GB {
		t.Fatalf("cluster listing publishes maxVramBytesPerGpu = %d, want the largest node's %d",
			list[0].MaxVRAMBytesPerGPU, itVRAM48GB)
	}
}

// Every way the check must refuse NOTHING. Each row is an outage it would
// otherwise cause: a huggingface.co incident, or a fleet whose agents have not
// reported GPU facts yet, stopping an entire organization from deploying model
// endpoints onto hardware it already owns.
func TestTheCreateTimeFitCheckFailsOpenOnBothTargets(t *testing.T) {
	for _, tc := range []struct {
		name string
		// size is what the sizer answers; nil means no sizer is wired at all.
		size   *store.ModelSize
		perGPU uint64
	}{
		{
			name:   "the model's parameter count is unknown, so there is nothing to compare",
			size:   &store.ModelSize{},
			perGPU: itVRAM24GB,
		},
		{
			name:   "the host has never reported a GPU inventory — absent is unknown, not zero",
			size:   func() *store.ModelSize { s := knownSize(itLlama70B, "~188 GB"); return &s }(),
			perGPU: 0,
		},
		{
			name:   "no sizer is configured at all, which is a supported control plane",
			perGPU: itVRAM24GB,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, _ := testStore(t)
			ctx := context.Background()
			orgID := "org_fit_open"
			_, envID, serverID := llmFitFixture(t, st, orgID, tc.perGPU)
			if tc.size != nil {
				st.SetModelSizer(stubSizer{size: *tc.size})
			}

			if _, err := createLLM(st, orgID, envID, serverID, "", "server-target",
				"meta-llama/Llama-3.1-70B-Instruct"); err != nil {
				t.Fatalf("server-targeted create was refused on an unprovable comparison: %v", err)
			}

			// The cluster branch has to fail open on exactly the same grounds,
			// plus one of its own: a cluster that has been declared but not built
			// out has no nodes, so nobody has said what it can run.
			cluster, err := st.CreateCluster(ctx, orgID, store.CreateClusterInput{
				EnvironmentID: envID, Name: "prod", ControlPlaneID: serverID,
			}, "admin")
			if err != nil {
				t.Fatal(err)
			}
			_, err = createLLM(st, orgID, envID, "", cluster.ID, "cluster-target",
				"meta-llama/Llama-3.1-70B-Instruct")
			assertNotAFitRefusal(t, err)
		})
	}
}

// The weights credential (SIGMA-213). The runtime catalog names
// HUGGING_FACE_HUB_TOKEN as a secret reference and the agent refuses to create a
// container the control plane will not answer that reference for, so what this
// pins is that the value exists and that a reference is only ever declared when
// it does.
func TestAnInferenceEndpointGetsTheControlPlanesHuggingFaceToken(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_hub_token"
	projectID, envID, serverID := llmFitFixture(t, st, orgID, itVRAM24GB)
	st.SetHuggingFaceToken("hf_from_the_control_plane")

	llm, err := createLLM(st, orgID, envID, serverID, "", "llama", "meta-llama/Llama-3.1-8B")
	if err != nil {
		t.Fatal(err)
	}

	// The agent's own audited fetch is where the value has to appear: this is
	// the call whose answer becomes the container's environment.
	secrets, err := st.ResolveSecretsForResource(ctx, orgID, serverID, llm.ID, "sigmad")
	if err != nil {
		t.Fatal(err)
	}
	var got store.ResolvedSecret
	for _, s := range secrets {
		if s.Name == store.HubTokenSecretName {
			got = s
		}
	}
	if got.Value != "hf_from_the_control_plane" {
		t.Fatalf("%s resolved to %q — the reference the runtime catalog renders has nothing behind it, "+
			"and the agent fails the container create on exactly that", store.HubTokenSecretName, got.Value)
	}
	if !got.EnvVar {
		t.Error("the token must arrive as an environment variable; the runtime reads no file")
	}

	targets, err := st.LLMTargetsForServer(ctx, serverID)
	if err != nil {
		t.Fatal(err)
	}
	if !targets[llm.ID].WeightsToken {
		t.Fatal("the endpoint does not declare its credential, so no reference is rendered for it")
	}

	// And the wizard is told the truth about the target BEFORE the operator
	// picks a gated model, which is the only moment the answer is worth anything.
	available, err := st.WeightsTokenAvailable(ctx, orgID, projectID)
	if err != nil {
		t.Fatal(err)
	}
	if !available {
		t.Fatal("a control plane holding a token reports no weights credential to the wizard")
	}
}

// A control plane with no token of its own must not claim one, and must not
// declare a reference nothing can resolve — a public model on a self-hosted
// control plane is the ordinary case, and it used to be a container that never
// started.
func TestWithoutATokenNothingIsClaimedAndNothingIsReferenced(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_no_hub_token"
	projectID, envID, serverID := llmFitFixture(t, st, orgID, itVRAM24GB)

	llm, err := createLLM(st, orgID, envID, serverID, "", "mistral", "mistralai/Mistral-7B-v0.1")
	if err != nil {
		t.Fatal(err)
	}
	targets, err := st.LLMTargetsForServer(ctx, serverID)
	if err != nil {
		t.Fatal(err)
	}
	if targets[llm.ID].WeightsToken {
		t.Fatal("an endpoint with no credential stored still declares one; the agent would fail its apply")
	}
	available, err := st.WeightsTokenAvailable(ctx, orgID, projectID)
	if err != nil {
		t.Fatal(err)
	}
	if available {
		t.Fatal("the wizard is told a gated model will download when nothing can authenticate the pull")
	}

	// The operator's own secret is the other way a weights token exists, and the
	// wizard has to see it — being blocked on the model step while holding the
	// credential that would have worked is the second half of the same defect.
	if _, err := st.CreateSecret(ctx, orgID, "admin", store.CreateSecretInput{
		ProjectID: projectID, Name: store.HubTokenSecretName,
		Value: "hf_the_operators_own", EnvVar: true,
	}); err != nil {
		t.Fatal(err)
	}
	available, err = st.WeightsTokenAvailable(ctx, orgID, projectID)
	if err != nil {
		t.Fatal(err)
	}
	if !available {
		t.Fatal("an org holding HUGGING_FACE_HUB_TOKEN is still told it has no weights credential")
	}

	// And theirs is the one that gets used. The database engines' generated
	// credentials deliberately win a name collision; this one deliberately loses,
	// because a token in the project is an operator naming the account that
	// fetches their weights.
	st.SetHuggingFaceToken("hf_from_the_control_plane")
	second, err := createLLM(st, orgID, envID, serverID, "", "qwen", "Qwen/Qwen2.5-7B")
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := st.ResolveSecretsForResource(ctx, orgID, serverID, second.ID, "sigmad")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, s := range secrets {
		if s.Name != store.HubTokenSecretName {
			continue
		}
		seen++
		if s.Value != "hf_the_operators_own" {
			t.Fatalf("%s resolved to the control plane's token, overriding the operator's own",
				store.HubTokenSecretName)
		}
	}
	if seen != 1 {
		t.Fatalf("%s resolved %d times; the agent injects one value per name and two is an argument "+
			"about which", store.HubTokenSecretName, seen)
	}
}
