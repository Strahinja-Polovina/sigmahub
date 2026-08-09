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

	"github.com/jackc/pgx/v5"
)

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
		SecretEnvNames: []string{"HUGGING_FACE_HUB_TOKEN"},
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
func (d LLMEngineDef) Command(model string) []string {
	switch d.Engine {
	case "vllm":
		if model == "" {
			return nil
		}
		return []string{"--model", model, "--host", "0.0.0.0", "--port", "8000"}
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
}

// provisionLLMTx allocates the resource's mesh-bound inference port and records
// its runtime + model. Runs inside CreateResource's transaction, symmetric with
// provisionDatabaseTx and provisionS3Tx.
func (s *Store) provisionLLMTx(ctx context.Context, tx pgx.Tx, orgID string, r Resource) error {
	spec := parseLLMSpec(r.Spec)
	engine := spec.Engine
	if engine == "" {
		engine = DefaultLLMEngine
	}
	if !IsLLMEngine(engine) {
		return ErrInvalid{Msg: fmt.Sprintf("unknown inference runtime %q", engine)}
	}
	port, err := allocateDBPort(ctx, tx, r.ServerID)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO llm_endpoints (resource_id, org_id, server_id, engine, model, port)
		VALUES ($1,$2,$3,$4,$5,$6)`,
		r.ID, orgID, r.ServerID, engine, spec.Model, port)
	return err
}

// LLMTargetsForServer returns the server's inference endpoints, keyed by
// resource id.
func (s *Store) LLMTargetsForServer(ctx context.Context, serverID string) (map[string]LLMTarget, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT resource_id, engine, model, port FROM llm_endpoints WHERE server_id = $1`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]LLMTarget{}
	for rows.Next() {
		var id string
		var t LLMTarget
		if err := rows.Scan(&id, &t.Engine, &t.Model, &t.Port); err != nil {
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
