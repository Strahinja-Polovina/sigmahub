package store

// GPU / model hosting.
//
// An `llm` resource is a model served over an OpenAI-compatible HTTP API by a
// runtime container on a GPU host. The catalog below is the single source of
// truth for the runtime image, its port and how a model reference becomes a
// start command — the reconciler renders from it and the endpoint readout is
// derived from it, so the two cannot drift.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/hf"
	"github.com/jackc/pgx/v5"
)

// HubTokenSecretName is the environment variable an inference runtime reads a
// Hugging Face credential from, and therefore the name of the secret carrying
// it.
//
// It is a package-level name because it now has to mean ONE thing in three
// places that used to disagree. CP_HUGGING_FACE_TOKEN authenticates the model
// picker's metadata calls in this process; this secret is what the agent
// injects so the runtime can DOWNLOAD the weights. Nothing populated the second
// from the first, so a control plane could have a perfectly good token and
// still 401 forty gigabytes into a pull on a GPU-billed host — while an
// operator who had set only the secret was told by the wizard to go configure
// the variable that would not have helped. Seeding (seedHubTokenTx) and
// reporting (WeightsTokenAvailable) both key off this constant so the three
// cannot drift apart again.
const HubTokenSecretName = "HUGGING_FACE_HUB_TOKEN"

// LLMEngineDef is one supported inference runtime.
type LLMEngineDef struct {
	Engine string
	// Image is pinned: a floating tag would let the runtime change under a
	// pinned DSD (and the agent policy refuses it anyway).
	Image         string
	ContainerPort int
	// ModelCacheMount is where downloaded weights live. Weights are big and
	// slow to fetch, so they get a named volume and survive a redeploy.
	ModelCacheMount string
	// SecretEnvNames are credentials rendered as secret REFERENCES and resolved
	// agent-side, so a captured DSD carries nothing.
	SecretEnvNames []string
}

// llmEngines is the runtime catalog. vLLM is the default: it is the runtime our
// own pricing comparison is built on and the one with an OpenAI-compatible
// server out of the box.
var llmEngines = map[string]LLMEngineDef{
	"vllm": {
		Engine:          "vllm",
		Image:           "vllm/vllm-openai:v0.6.6",
		ContainerPort:   8000,
		ModelCacheMount: "/root/.cache/huggingface",
		// Gated models (Llama & co) need a Hugging Face token to download.
		SecretEnvNames: []string{HubTokenSecretName},
	},
	"ollama": {
		Engine:          "ollama",
		Image:           "ollama/ollama:0.5.4",
		ContainerPort:   11434,
		ModelCacheMount: "/root/.ollama",
	},
}

// DefaultLLMEngine is what an `llm` resource gets when its spec names none.
const DefaultLLMEngine = "vllm"

// LLMEngine returns a runtime definition.
func LLMEngine(engine string) (LLMEngineDef, bool) {
	def, ok := llmEngines[engine]
	return def, ok
}

// IsLLMKind reports whether a resource kind is model hosting.
func IsLLMKind(kind string) bool { return kind == "llm" }

// IsLLMEngine reports whether a runtime name is known.
func IsLLMEngine(engine string) bool { _, ok := llmEngines[engine]; return ok }

// LLMEngineNames lists the runtimes, for the API to publish to the dashboard.
func LLMEngineNames() []string {
	out := make([]string, 0, len(llmEngines))
	for name := range llmEngines {
		out = append(out, name)
	}
	return out
}

// Command builds the runtime's start command for a model reference. vLLM takes
// the model as a flag; ollama serves and pulls on first request, so the model
// rides the environment instead.
//
// contextTokens is the window this endpoint may be served at, already decided by
// hf.ServedContextTokens and stored on the endpoint (LLMTarget.ContextTokens).
// It is an ARGUMENT and not a constant because both ends of the range are fatal.
// Unpinned, vLLM takes the model's own max_position_embeddings — 128k on the
// Llama 3.1 family, sixteen times what the fit check's KV-cache term budgets —
// so the check approves the model onto a card and the runtime then demands a KV
// cache nobody estimated. Pinned to hf.SizedContextTokens unconditionally, which
// is what this used to render, vLLM refuses to start any model whose own ceiling
// is SHORTER: TinyLlama-1.1B-Chat (2048) and Llama-2-13B-chat-AWQ (4096) both
// exited at startup with "User-specified max_model_len is greater than the
// derived max_model_len", after being approved and drawn green everywhere.
//
// 0 means the model's ceiling is unknown, and the flag is then OMITTED
// entirely — the runtime derives its window from the weights it pulled, which is
// the only honest answer when the Hub could not be asked. That is the same
// fail-open direction the fit check takes on the same input.
func (d LLMEngineDef) Command(model string, contextTokens int) []string {
	switch d.Engine {
	case "vllm":
		if model == "" {
			return nil
		}
		cmd := []string{"--model", model, "--host", "0.0.0.0", "--port", "8000"}
		if contextTokens > 0 {
			cmd = append(cmd, "--max-model-len", strconv.Itoa(contextTokens))
		}
		return cmd
	default:
		return nil
	}
}

// PlainEnv is the runtime's non-secret environment.
func (d LLMEngineDef) PlainEnv(model string) map[string]string {
	if d.Engine == "ollama" && model != "" {
		return map[string]string{"OLLAMA_MODEL": model, "OLLAMA_HOST": "0.0.0.0:11434"}
	}
	return nil
}

// EndpointURL is the OpenAI-compatible base URL clients dial over the mesh.
func (d LLMEngineDef) EndpointURL(host string, port int) string {
	if host == "" {
		return ""
	}
	if d.Engine == "ollama" {
		return fmt.Sprintf("http://%s:%d", host, port)
	}
	return fmt.Sprintf("http://%s:%d/v1", host, port)
}

// LLMTarget is one provisioned inference endpoint, for the reconciler.
type LLMTarget struct {
	Engine string
	Model  string
	Port   int
	// WeightsToken is true when this endpoint has a Hugging Face credential
	// stored for it, and it decides whether the reconciler renders the
	// HUGGING_FACE_HUB_TOKEN reference at all.
	//
	// It has to be a decision rather than a constant because the agent treats an
	// unresolvable reference as fatal — "secret %q referenced but not provided
	// by the control plane" — so a runtime catalog that names the secret
	// unconditionally turned the most ordinary case there is (a public model on
	// a control plane holding no Hub token) into a container that never started.
	WeightsToken bool
	// ContextTokens is the window this endpoint is started at, decided at
	// provision from the model's own ceiling (hf.ServedContextTokens) and stored
	// so a render never has to call huggingface.co. 0 means the ceiling was never
	// known and Command renders no --max-model-len at all.
	ContextTokens int
}

// hubTokenAAD binds a stored Hugging Face token to its endpoint row, exactly as
// dbAAD binds a generated database password to its resource: a ciphertext
// copied into another org's or another resource's row fails to open.
func hubTokenAAD(orgID, resourceID string) []byte { return []byte(orgID + "|llm|" + resourceID) }

// provisionLLMTx allocates the resource's mesh-bound inference port, records
// its runtime, model and served context window, and seeds the weights
// credential. Runs inside CreateResource's transaction, symmetric with
// provisionDatabaseTx and provisionS3Tx.
//
// size is what the Hub said about this model a moment ago, and the only part of
// it that outlives the create is the context window: the fit check is a decision
// made here and then over, while --max-model-len has to be rendered into every
// document for as long as the endpoint exists. Resolving it now rather than at
// render time is what keeps huggingface.co off the agent's poll path.
//
// It is server-scoped by construction — allocateDBPort numbers ports per server
// and llm_endpoints.server_id is NOT NULL REFERENCES servers(id) — which is why
// clusterExcludedKinds refuses an `llm` aimed at a cluster before it can reach
// this function with an empty server id.
func (s *Store) provisionLLMTx(ctx context.Context, tx pgx.Tx, orgID string, r Resource, size ModelSize) error {
	spec := parseLLMSpec(r.Spec)
	engine := spec.Engine
	if engine == "" {
		engine = DefaultLLMEngine
	}
	def, ok := LLMEngine(engine)
	if !ok {
		return ErrInvalid{Msg: fmt.Sprintf("unknown inference runtime %q", engine)}
	}
	port, err := allocateDBPort(ctx, tx, r.ServerID)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO llm_endpoints (resource_id, org_id, server_id, engine, model, port, context_tokens)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		r.ID, orgID, r.ServerID, engine, spec.Model, port,
		hf.ServedContextTokens(size.MaxPositionEmbeddings)); err != nil {
		return err
	}
	return s.seedHubTokenTx(ctx, tx, orgID, def, r)
}

// operatorHubTokenClause matches an operator's own HUGGING_FACE_HUB_TOKEN
// secrets that would ACTUALLY resolve for a resource in one environment, and it
// is one constant because two callers ask that question at two moments and must
// not answer it differently.
//
// They did. seedHubTokenTx carried the environment predicate — a secret scoped
// to staging does not reach production, which is what ResolveSecretsForResource
// enforces when the agent drains the value — and WeightsTokenAvailable had none.
// So an org whose only Hub token was scoped to staging was told by the wizard
// that a gated model would download, in production, where nothing resolves it:
// the create was accepted, the control plane seeded nothing because it holds no
// token of its own, and the pull 401'd tens of gigabytes in on a GPU-billed
// host. That is SIGMA-213's own defect re-created by a missing WHERE clause.
//
// $1 org, $2 project, $3 secret name, $4 environment. The query it is spliced
// into must read from `secrets`. An EMPTY $4 matches org-wide secrets only
// (environment_id IS NULL): the caller has not said which environment it is
// asking about, and a secret pinned to one is not an answer about another.
const operatorHubTokenClause = `
		org_id = $1 AND project_id = $2 AND lower(name) = lower($3)
		  AND (environment_id IS NULL OR environment_id = $4)`

// seedHubTokenTx stores this control plane's Hugging Face token as the
// endpoint's weights credential, encrypted under the org DEK exactly the way
// provisionDatabaseTx stores a generated database password.
//
// The reference was the whole feature and the value was nobody's job: the
// runtime catalog rendered HUGGING_FACE_HUB_TOKEN, the agent refused to start a
// container the control plane would not answer that reference for, and no code
// path had ever created the secret. An operator who had made a project secret
// of exactly that name by hand got a working deploy; everyone else got a
// container that never started, or — for a gated model the picker's token could
// see and the download could not — a 401 tens of gigabytes into a pull on a
// host billed at GPU rates.
//
// Nothing is stored when the operator's own secret of that name already reaches
// this resource. Theirs is a deliberate statement about which Hugging Face
// account fetches their weights, and keeping a second copy of ours beside it
// would leave two answers to one question; ResolveSecretsForResource holds the
// same precedence for a secret created after this point.
func (s *Store) seedHubTokenTx(ctx context.Context, tx pgx.Tx, orgID string, def LLMEngineDef, r Resource) error {
	// ollama pulls from its own library and names no credential, so there is
	// nothing here for it to need — asking the catalog rather than assuming vLLM
	// keeps this true for the next runtime added to it.
	if s.hubToken == "" || !slices.Contains(def.SecretEnvNames, HubTokenSecretName) {
		return nil
	}
	var operatorHasOne bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM secrets WHERE`+operatorHubTokenClause+`)`,
		orgID, r.ProjectID, HubTokenSecretName, r.EnvironmentID).Scan(&operatorHasOne); err != nil {
		return fmt.Errorf("look up hugging face token secret: %w", err)
	}
	if operatorHasOne {
		return nil
	}
	dekID, dek, err := s.activeDEKTx(ctx, tx, orgID)
	if err != nil {
		return err
	}
	nonce, ct, err := gcmSeal(dek, hubTokenAAD(orgID, r.ID), []byte(s.hubToken))
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE llm_endpoints
		   SET token_ciphertext = $2, token_nonce = $3, token_dek_id = $4
		 WHERE resource_id = $1`, r.ID, ct, nonce, dekID); err != nil {
		return fmt.Errorf("store hugging face token: %w", err)
	}
	return nil
}

// resolveLLMSecretsTx appends the endpoint's weights credential as an env-mode
// resolved secret — the same audited channel resolveDBSecretsTx uses for a
// database password. Returns nil, nil for anything that is not a seeded
// inference endpoint on this server.
//
// operatorProvided names the secrets the tenant's own scope already resolved,
// and this credential LOSES a collision with them. That is the opposite of the
// database rule (those are appended last precisely so the engine's names win),
// and deliberately so: a HUGGING_FACE_HUB_TOKEN secret in the project is an
// operator naming the account that fetches their weights, and quietly replacing
// it with the control plane's token turns their own private repository into a
// 404 they have no way to explain.
func (s *Store) resolveLLMSecretsTx(ctx context.Context, tx pgx.Tx, orgID, serverID, resourceID string, operatorProvided map[string]bool) ([]ResolvedSecret, error) {
	if operatorProvided[HubTokenSecretName] {
		return nil, nil
	}
	var (
		dekID     string
		ct, nonce []byte
	)
	// server_id scoping mirrors ResolveSecretsForResource: an agent token only
	// ever drains credentials for resources scheduled onto ITS server.
	err := tx.QueryRow(ctx, `
		SELECT token_dek_id, token_ciphertext, token_nonce
		  FROM llm_endpoints
		 WHERE org_id = $1 AND resource_id = $2 AND server_id = $3
		   AND token_ciphertext IS NOT NULL`,
		orgID, resourceID, serverID).Scan(&dekID, &ct, &nonce)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	dek, err := s.dekPlaintext(ctx, tx, dekID)
	if err != nil {
		return nil, err
	}
	plaintext, err := gcmOpen(dek, hubTokenAAD(orgID, resourceID), nonce, ct)
	if err != nil {
		return nil, fmt.Errorf("decrypt hugging face token: %w", err)
	}
	return []ResolvedSecret{{Name: HubTokenSecretName, Value: string(plaintext), EnvVar: true}}, nil
}

// WeightsTokenAvailable reports whether an `llm` resource created in this
// project would have a Hugging Face credential to fetch its weights with.
//
// This is what the wizard's tokenConfigured means, and the distinction it draws
// is the difference between two opposite wrong answers. The picker's token
// authenticates metadata calls inside THIS process; it says nothing about a
// download performed by an agent on a GPU host. A wizard gated on the picker's
// token waved a gated model through to a 401 mid-pull, while an operator who
// had put a HUGGING_FACE_HUB_TOKEN secret in their project — the thing that
// actually fetches the weights — was blocked on the model step and told to set
// a variable that would not have changed anything.
//
// A control-plane token counts, because CreateResource seeds it (see
// seedHubTokenTx). An empty projectID answers on that half alone: the caller
// has not said which project it is asking about, and inventing one would be a
// guess about someone's secrets.
//
// environmentID narrows the operator's half to the secrets that would actually
// reach a resource created there, through operatorHubTokenClause — the SAME
// predicate the seeding path applies, because the wizard must not promise a
// token the create then cannot find. An empty environmentID counts only
// org-wide secrets, for the same reason an empty projectID counts none: an
// unstated scope is not permission to assume the widest one.
func (s *Store) WeightsTokenAvailable(ctx context.Context, orgID, projectID, environmentID string) (bool, error) {
	if s.hubToken != "" {
		return true, nil
	}
	if projectID == "" {
		return false, nil
	}
	var exists bool
	err := s.Pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM secrets WHERE`+operatorHubTokenClause+`)`,
		orgID, projectID, HubTokenSecretName, environmentID).Scan(&exists)
	return exists, err
}

// LLMTargetsForServer returns the server's inference endpoints, keyed by
// resource id.
func (s *Store) LLMTargetsForServer(ctx context.Context, serverID string) (map[string]LLMTarget, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT resource_id, engine, model, port, token_ciphertext IS NOT NULL, context_tokens
		  FROM llm_endpoints WHERE server_id = $1`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]LLMTarget{}
	for rows.Next() {
		var id string
		var t LLMTarget
		if err := rows.Scan(&id, &t.Engine, &t.Model, &t.Port, &t.WeightsToken, &t.ContextTokens); err != nil {
			return nil, err
		}
		out[id] = t
	}
	return out, rows.Err()
}

// LLMInfo is the dashboard readout for a model endpoint.
type LLMInfo struct {
	Engine   string `json:"engine"`
	Model    string `json:"model"`
	Image    string `json:"image"`
	Host     string `json:"host"` // mesh IP; empty until enrollment
	Port     int    `json:"port"`
	Endpoint string `json:"endpoint"`
}

// GetLLM returns the resource's inference endpoint readout.
func (s *Store) GetLLM(ctx context.Context, orgID, resourceID string) (LLMInfo, error) {
	var info LLMInfo
	var host *string
	err := s.Pool.QueryRow(ctx, `
		SELECT e.engine, e.model, e.port, sv.mesh_ip
		  FROM llm_endpoints e JOIN servers sv ON sv.id = e.server_id
		 WHERE e.org_id = $1 AND e.resource_id = $2`, orgID, resourceID).
		Scan(&info.Engine, &info.Model, &info.Port, &host)
	if errors.Is(err, pgx.ErrNoRows) {
		return LLMInfo{}, ErrNotFound
	}
	if err != nil {
		return LLMInfo{}, err
	}
	if host != nil {
		info.Host = *host
	}
	if def, ok := LLMEngine(info.Engine); ok {
		info.Image = def.Image
		info.Endpoint = def.EndpointURL(info.Host, info.Port)
	}
	return info, nil
}
