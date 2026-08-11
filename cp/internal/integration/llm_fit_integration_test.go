package integration

// The create-time VRAM fit check and the weights credential, driven through
// CreateResource against a real database (SIGMA-213, SIGMA-214).
//
// The unit tests in cp/internal/store call checkModelFits, checkModelServable
// and sizeModelForFit directly. They stayed green when the checkModelFits call
// was deleted from domain.go, which is to say they proved the arithmetic and
// nothing about the feature: the rule only exists where CreateResource applies
// it. So these drive the real store, with the real SQL that reads a host's
// facts, and each one fails if its call site goes away.
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

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/hf"
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

// knownSize is a model the control plane could size. The zero store.ModelSize is
// every way it could not (Hub down, no safetensors index, an Ollama tag, no
// sizer), which is why the fail-open table below can state them all as one value.
//
// The sentence is RENDERED by hf.FormatVRAM rather than typed beside the byte
// count: the refusal quotes VRAMText verbatim, so a hand-written string here
// would let this file assert a spelling the control plane cannot produce — which
// it did, for as long as these said "~21 GB" and FormatVRAM said "~21.4 GB".
func knownSize(bytes uint64) store.ModelSize {
	return store.ModelSize{
		ParametersKnown: true, VRAMBytesRequired: bytes, VRAMText: hf.FormatVRAM(bytes),
	}
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
	st.SetModelSizer(stubSizer{size: knownSize(itLlama70B)})

	_, err := createLLM(st, orgID, envID, serverID, "", "big", "meta-llama/Llama-3.1-70B-Instruct")
	var invalid store.ErrInvalid
	if !errors.As(err, &invalid) {
		t.Fatalf("creating a 188 GB model on a 24 GB card err = %v, want ErrInvalid (a 422) — "+
			"the create-time re-check is not running on the server branch", err)
	}
	// Both numbers and the model, because a refusal that names one of them
	// leaves the operator guessing whether to change the model or the machine.
	for _, want := range []string{
		"meta-llama/Llama-3.1-70B-Instruct", hf.FormatVRAM(itLlama70B), "gpu-hel-01", "23 GB",
	} {
		if !strings.Contains(invalid.Msg, want) {
			t.Errorf("refusal does not mention %q: %s", want, invalid.Msg)
		}
	}

	// And the remedy it names actually works: the quantized build of the same
	// model fits the same card, so the sentence is advice rather than consolation.
	st.SetModelSizer(stubSizer{size: knownSize(itLlama70Q)})
	if _, err := createLLM(st, orgID, envID, serverID, "", "quantized",
		"TheBloke/Llama-3.1-70B-AWQ"); err == nil {
		t.Fatal("a 47 GB model was accepted onto a 24 GB card")
	}
	st.SetModelSizer(stubSizer{size: knownSize(itVRAM24GB)})
	if _, err := createLLM(st, orgID, envID, serverID, "", "exact",
		"some/exactly-sized"); err != nil {
		t.Fatalf("a model sized exactly to the card was refused: %v", err)
	}
}

// An `llm` aimed at a cluster is refused for the kind, and refused EARLY.
//
// This is the shape of the defect it closes, not a hypothetical: the wizard
// offered every GPU cluster as an eligible target, the fit check passed it green
// against the cluster's largest card, CreateResource accepted it, and then
// provisionLLMTx allocated a port against an empty server id and the insert died
// on llm_endpoints_server_id_fkey — SQLSTATE 23503, surfaced to the operator as
// a 500 naming a database constraint. Nothing renders a cluster-targeted model
// endpoint either, so even a create that survived would have produced an
// endpoint that never existed anywhere.
func TestAimingAModelAtAClusterIsRefusedByKindAndNotByAConstraint(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_llm_cluster"
	_, envID, controlPlane := llmFitFixture(t, st, orgID, itVRAM48GB)

	cluster, err := st.CreateCluster(ctx, orgID, store.CreateClusterInput{
		EnvironmentID: envID, Name: "prod", ControlPlaneID: controlPlane,
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}

	// A model that comfortably fits the cluster's card, so the refusal below
	// cannot be the fit check wearing a different hat.
	st.SetModelSizer(stubSizer{size: knownSize(itLlama70Q)})
	_, err = createLLM(st, orgID, envID, "", cluster.ID, "llama", "TheBloke/Llama-3.1-70B-AWQ")

	var notClusterable store.ErrKindNotClusterable
	if !errors.As(err, &notClusterable) {
		t.Fatalf("aiming an llm at a cluster err = %v, want ErrKindNotClusterable — a create that "+
			"gets past this reaches provisionLLMTx with no server and dies on a foreign key", err)
	}
	// The sentence the dashboard already renders for every other excluded kind,
	// so the operator is told where the model DOES go.
	if !strings.Contains(notClusterable.Error(), "runs on its own server") {
		t.Errorf("refusal does not say where an llm runs instead: %s", notClusterable.Error())
	}
	// And the published list is the same rule, because the wizard draws its
	// eligible targets from it rather than keeping a second copy.
	excluded := map[string]bool{}
	for _, k := range store.ClusterExcludedKinds() {
		excluded[k] = true
	}
	if !excluded["llm"] {
		t.Fatalf("the API publishes %v, so the wizard still offers clusters for llm and the "+
			"operator meets this refusal only after Review", store.ClusterExcludedKinds())
	}
}

// Every way the create-time checks must refuse NOTHING. Each row is an outage
// they would otherwise cause: a huggingface.co incident, or a fleet whose agents
// have not reported GPU facts yet, stopping an entire organization from
// deploying model endpoints onto hardware it already owns.
func TestTheCreateTimeModelChecksFailOpenOnEveryUnknown(t *testing.T) {
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
			size:   func() *store.ModelSize { s := knownSize(itLlama70B); return &s }(),
			perGPU: 0,
		},
		{
			// The gated-repo shape: the Hub answered, but with no task and no
			// format. Reading either absence as a refusal would block every gated
			// model on a control plane holding no Hub token.
			name: "the repository resolved but the Hub described neither its task nor its format",
			size: func() *store.ModelSize {
				s := knownSize(itLlama70Q)
				return &s
			}(),
			perGPU: itVRAM48GB,
		},
		{
			name:   "no sizer is configured at all, which is a supported control plane",
			perGPU: itVRAM24GB,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, _ := testStore(t)
			orgID := "org_fit_open"
			_, envID, serverID := llmFitFixture(t, st, orgID, tc.perGPU)
			if tc.size != nil {
				st.SetModelSizer(stubSizer{size: *tc.size})
			}

			if _, err := createLLM(st, orgID, envID, serverID, "", "server-target",
				"meta-llama/Llama-3.1-70B-Instruct"); err != nil {
				t.Fatalf("create was refused on an unprovable check: %v", err)
			}
		})
	}
}

// The two refusals the wizard used to own alone. An API-direct create is the
// whole reason they exist CP-side: a GGUF repository routed to vLLM starts a
// container that reports HEALTHY and 404s every completion, which nothing in
// the product watches for, on a host billed at GPU rates the entire time.
func TestAModelNoRuntimeHereCanServeIsRefusedByTheAPIAndNotOnlyTheWizard(t *testing.T) {
	st, _ := testStore(t)
	orgID := "org_servable"
	_, envID, serverID := llmFitFixture(t, st, orgID, itVRAM48GB)

	gguf := knownSize(itLlama70Q)
	gguf.Quantization = "gguf"
	st.SetModelSizer(stubSizer{size: gguf})
	_, err := createLLM(st, orgID, envID, serverID, "", "gguf", "TheBloke/phi-2-GGUF")
	var invalid store.ErrInvalid
	if !errors.As(err, &invalid) {
		t.Fatalf("creating a GGUF model err = %v, want ErrInvalid (a 422) — vLLM cannot open one, "+
			"so this create buys a healthy container that answers nothing", err)
	}
	if !strings.Contains(invalid.Msg, "TheBloke/phi-2-GGUF") {
		t.Errorf("refusal does not name the model: %s", invalid.Msg)
	}

	embedding := knownSize(itLlama70Q)
	embedding.PipelineTag = "sentence-similarity"
	st.SetModelSizer(stubSizer{size: embedding})
	_, err = createLLM(st, orgID, envID, serverID, "", "embed", "sentence-transformers/all-MiniLM-L6-v2")
	if !errors.As(err, &invalid) {
		t.Fatalf("creating an embedding model as an llm err = %v, want ErrInvalid", err)
	}

	// And the task the runtime IS for still goes through, or the refusal above
	// would be a rule against deploying anything.
	text := knownSize(itLlama70Q)
	text.PipelineTag = hf.TextGenerationTask
	st.SetModelSizer(stubSizer{size: text})
	if _, err := createLLM(st, orgID, envID, serverID, "", "chat",
		"TheBloke/Llama-3.1-70B-AWQ"); err != nil {
		t.Fatalf("a text-generation model was refused: %v", err)
	}
}

// The context window the endpoint is served at is decided at create and stored,
// because the reconciler renders --max-model-len from it on every poll and must
// never call huggingface.co to do so.
func TestTheServedContextIsClampedToTheModelsOwnCeilingAtCreate(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		maxPositionEmbeddings int
		want                  int
	}{
		// The failure this replaces: pinned to 8192 unconditionally, a 2048-token
		// model exits at startup on a card it fits three times over.
		{"a model shorter than the window we sized", 2048, 2048},
		{"a model longer than the window we sized", 131072, 8192},
		// Unknown gets the window the VRAM estimate was paid for, so the flag
		// and the arithmetic agree. Rendering nothing here was the OTHER way of
		// getting this wrong: a ceiling is unreadable when config.json is gated,
		// gated repositories are the long-context ones, and an unpinned Llama
		// takes 131072 and exits on a KV cache the fit check never estimated.
		{"a model whose ceiling nobody could read", 0, hf.SizedContextTokens},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, _ := testStore(t)
			ctx := context.Background()
			orgID := "org_ctx"
			_, envID, serverID := llmFitFixture(t, st, orgID, itVRAM48GB)

			size := knownSize(itLlama70Q)
			size.MaxPositionEmbeddings = tc.maxPositionEmbeddings
			st.SetModelSizer(stubSizer{size: size})

			llm, err := createLLM(st, orgID, envID, serverID, "", "chat", "TheBloke/Llama-3.1-70B-AWQ")
			if err != nil {
				t.Fatal(err)
			}
			targets, err := st.LLMTargetsForServer(ctx, serverID)
			if err != nil {
				t.Fatal(err)
			}
			if got := targets[llm.ID].ContextTokens; got != tc.want {
				t.Fatalf("endpoint serves %d context tokens, want %d; the reconciler renders "+
					"--max-model-len from this and vLLM refuses to start above the model's own limit",
					got, tc.want)
			}
		})
	}
}

// The weights credential, after SIGMA-302 took the operator's account out of it.
//
// CP_HUGGING_FACE_TOKEN used to be sealed into every tenant's llm_endpoints row
// at create and handed to the agent as an env-mode secret, so one operator-owned
// Hub credential — valid across every tenant and every gated repo the operator
// had accepted terms for — landed in a container's environment on a GPU host the
// customer owns and has a shell on. This pins that it does not any more, at the
// one call whose answer becomes that environment.
func TestTheControlPlanesHuggingFaceTokenNeverReachesATenantsContainer(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_hub_token"
	projectID, envID, serverID := llmFitFixture(t, st, orgID, itVRAM24GB)

	llm, err := createLLM(st, orgID, envID, serverID, "", "llama", "meta-llama/Llama-3.1-8B")
	if err != nil {
		t.Fatal(err)
	}

	secrets, err := st.ResolveSecretsForResource(ctx, orgID, serverID, llm.ID, "sigmad")
	if err != nil {
		t.Fatal(err)
	}
	for _, sec := range secrets {
		if sec.Name == store.HubTokenSecretName {
			t.Fatalf("%s was delivered to the tenant with value %q — the control plane's own Hub "+
				"account must never leave this process", store.HubTokenSecretName, sec.Value)
		}
	}

	// Nothing is stored against the endpoint either, so no reference is rendered
	// and the agent is never asked to resolve one it cannot get.
	targets, err := st.LLMTargetsForServer(ctx, serverID)
	if err != nil {
		t.Fatal(err)
	}
	if targets[llm.ID].WeightsToken {
		t.Fatal("the endpoint still declares a stored weights credential, so the reconciler renders a reference for it")
	}

	// And the wizard says so rather than promising a download it cannot make.
	// Answering "yes" on the strength of a token the container will never see is
	// SIGMA-213's original defect, which is why the short-circuit had to go in
	// the same change as the seeding.
	available, err := st.WeightsTokenAvailable(ctx, orgID, projectID, envID)
	if err != nil {
		t.Fatal(err)
	}
	if available {
		t.Fatal("the wizard reports a weights credential this tenant does not have, so a gated model " +
			"will be approved and then 401 mid-pull on a GPU-billed host")
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
	available, err := st.WeightsTokenAvailable(ctx, orgID, projectID, envID)
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
	available, err = st.WeightsTokenAvailable(ctx, orgID, projectID, envID)
	if err != nil {
		t.Fatal(err)
	}
	if !available {
		t.Fatal("an org holding HUGGING_FACE_HUB_TOKEN is still told it has no weights credential")
	}

	// And theirs is the one that gets used — the only one there is, now that the
	// control plane seeds nothing (SIGMA-302). It must still arrive exactly once:
	// the agent injects one value per name, and two is an argument about which.
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
			t.Fatalf("%s resolved to %q rather than the operator's own secret",
				store.HubTokenSecretName, s.Value)
		}
	}
	if seen != 1 {
		t.Fatalf("%s resolved %d times; the agent injects one value per name and two is an argument "+
			"about which", store.HubTokenSecretName, seen)
	}
}

// "A token is available" and "a token would be seeded here" have to be the same
// question, asked with the same scope.
//
// They were not: the seeding path carried an environment predicate and the
// wizard's report carried none, so a HUGGING_FACE_HUB_TOKEN pinned to staging
// told an operator creating in PRODUCTION that their gated model would download.
// Nothing resolves that secret in production, so the create was accepted, the
// control plane seeded nothing, and the pull 401'd tens of gigabytes in on a
// host billed at GPU rates — SIGMA-213's own defect, re-created by a missing
// WHERE clause. Both callers now share operatorHubTokenClause.
func TestAStagingScopedTokenIsNotReportedAsAvailableInProduction(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_token_scope"
	projectID, prodEnv, _ := llmFitFixture(t, st, orgID, itVRAM24GB)

	staging, err := st.CreateEnvironment(ctx, orgID, projectID, "staging", false, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateSecret(ctx, orgID, "admin", store.CreateSecretInput{
		ProjectID: projectID, EnvironmentID: staging.ID, Name: store.HubTokenSecretName,
		Value: "hf_staging_only", EnvVar: true,
	}); err != nil {
		t.Fatal(err)
	}

	available, err := st.WeightsTokenAvailable(ctx, orgID, projectID, prodEnv)
	if err != nil {
		t.Fatal(err)
	}
	if available {
		t.Fatal("a token scoped to staging was reported as available for a create in production, " +
			"where ResolveSecretsForResource will never hand it to the runtime")
	}

	// The environment it IS scoped to gets the true answer, or this would be a
	// refusal to see the operator's token at all.
	available, err = st.WeightsTokenAvailable(ctx, orgID, projectID, staging.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !available {
		t.Fatal("the environment the token is scoped to is told it has no weights credential")
	}

	// An unstated environment counts org-wide secrets only. The caller has not
	// said which environment it is asking about, and answering yes off a secret
	// pinned to one of them is the same guess that started this.
	available, err = st.WeightsTokenAvailable(ctx, orgID, projectID, "")
	if err != nil {
		t.Fatal(err)
	}
	if available {
		t.Fatal("an unscoped question was answered from an environment-scoped secret")
	}
}
