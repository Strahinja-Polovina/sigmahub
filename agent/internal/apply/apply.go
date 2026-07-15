package apply

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/agent/internal/dsd"
)

// Handler executes one op of a given kind. Handlers must be idempotent — the
// same op may be re-applied after a resync. Returning an error marks the op
// failed and skips its dependents.
type Handler func(ctx context.Context, op dsd.Op) error

// Registry maps op kinds to handlers. It is the single enforcement point for
// the "no generic run-shell" invariant: an op whose kind is not registered is
// rejected, so a new capability can only be added by registering a typed
// handler here (P1-3 adds the container ops).
type Registry struct {
	handlers map[string]Handler
}

func NewRegistry() *Registry { return &Registry{handlers: map[string]Handler{}} }

func (r *Registry) Register(kind string, h Handler) { r.handlers[kind] = h }

// Apply runs a DSD's ops in dependency order, resuming from the journal so
// completed ops are not re-run, skipping dependents of a failed prerequisite,
// and recording every result. It returns the per-op results for status
// reporting. lastAppliedVersion is advanced only after every op is processed,
// so a crash mid-DSD re-enters here and resumes.
func (r *Registry) Apply(ctx context.Context, log *slog.Logger, j *Journal, doc dsd.Document) (map[string]OpResult, error) {
	results := make(map[string]OpResult, len(doc.Ops))
	failed := make(map[string]bool) // op ids failed or skipped this pass

	ordered, unresolved := dsd.TopoOrder(doc.Ops)
	for _, id := range unresolved {
		res := OpResult{Version: doc.Version, OpID: id, State: "failed", Err: "unorderable (cycle or missing dependency)", At: time.Now()}
		if err := j.Record(res); err != nil {
			return nil, err
		}
		results[id] = res
		failed[id] = true
	}

	for _, op := range ordered {
		// Resume: an op already applied at this version is not re-run.
		if prev, ok, err := j.Result(doc.Version, op.ID); err != nil {
			return nil, err
		} else if ok && prev.State == "applied" {
			results[op.ID] = prev
			continue
		}

		// Dependent of a failed/skipped prerequisite → skip.
		skip := false
		for _, dep := range op.DependsOn {
			if failed[dep] {
				skip = true
				break
			}
		}
		if skip {
			res := OpResult{Version: doc.Version, OpID: op.ID, State: "skipped", Err: "prerequisite failed", At: time.Now()}
			if err := j.Record(res); err != nil {
				return nil, err
			}
			results[op.ID] = res
			failed[op.ID] = true
			continue
		}

		h := r.handlers[op.Kind]
		res := OpResult{Version: doc.Version, OpID: op.ID, State: "applied", At: time.Now()}
		if h == nil {
			res.State = "failed"
			res.Err = fmt.Sprintf("unknown op kind %q", op.Kind)
		} else if err := h(ctx, op); err != nil {
			res.State = "failed"
			res.Err = err.Error()
		}
		if err := j.Record(res); err != nil {
			return nil, err
		}
		results[op.ID] = res
		if res.State != "applied" {
			failed[op.ID] = true
			log.Warn("op failed", "version", doc.Version, "op", op.ID, "err", res.Err)
		}
	}

	if err := j.SetLastAppliedVersion(doc.Version); err != nil {
		return nil, err
	}
	return results, nil
}

// StatusPayload shapes the per-op results for POST /v1/agent/dsd/status.
func StatusPayload(results map[string]OpResult) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(results))
	for id, res := range results {
		b, _ := json.Marshal(map[string]any{
			"state":      res.State,
			"error":      res.Err,
			"appliedAt":  res.At,
			"dsdVersion": res.Version,
		})
		out[id] = b
	}
	return out
}
