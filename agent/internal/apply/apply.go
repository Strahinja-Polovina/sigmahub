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

// DefaultOpTimeout bounds any op kind without an explicit entry in
// defaultOpTimeouts. Nothing the agent applies should hold the (serial) apply
// loop for a quarter of an hour, and an op that does is far more likely wedged
// than working.
const DefaultOpTimeout = 15 * time.Minute

// defaultOpTimeouts are the per-kind deadline overrides (SIGMA-339). Before
// this table Apply handed every handler the caller's context, and the DSD loop
// hands Apply the process root context — so a handler that never returns wedged
// the host forever. The Docker client is deliberately built with Timeout: 0 so a
// slow pull or build is not cut off mid-layer, which also means ImagePull /
// ImageBuild / ContainerCreate can block indefinitely against a black-holing
// registry; handlers that shell out (backup, s3ops) inherited the same unbounded
// context. The apply loop is serial, so one wedged op stops the host converging
// on ANYTHING — every later DSD version (a secret rotation, a firewall change,
// an unrelated app's deploy) queues behind it silently and forever. A deadline
// turns that into an honest journalled failure and lets the loop continue.
//
// The values are ceilings on pathology, not budgets: each is comfortably above
// what the op takes when it is working, and above any shorter timeout the
// handler already imposes on itself (backup's own opTimeout is 25m, so its
// ceiling here must be higher or this deadline would pre-empt it and mask the
// handler's better error).
var defaultOpTimeouts = map[string]time.Duration{
	"image.build":         30 * time.Minute, // multi-stage builds on a small host
	"image.pull":          15 * time.Minute,
	"backup.run":          30 * time.Minute, // handler self-limits at 25m
	"backup.base":         30 * time.Minute,
	"backup.verify":       30 * time.Minute,
	"backup.restore":      30 * time.Minute,
	"backup.restore-pitr": 30 * time.Minute,
	"k8s.node":            30 * time.Minute, // k3s install downloads and starts an API server
}

// Registry maps op kinds to handlers. It is the single enforcement point for
// the "no generic run-shell" invariant: an op whose kind is not registered is
// rejected, so a new capability can only be added by registering a typed
// handler here (P1-3 adds the container ops).
type Registry struct {
	handlers map[string]Handler
	timeouts map[string]time.Duration
}

func NewRegistry() *Registry {
	timeouts := make(map[string]time.Duration, len(defaultOpTimeouts))
	for k, d := range defaultOpTimeouts {
		timeouts[k] = d
	}
	return &Registry{handlers: map[string]Handler{}, timeouts: timeouts}
}

func (r *Registry) Register(kind string, h Handler) { r.handlers[kind] = h }

// SetTimeout overrides the deadline budget for one op kind. Handlers that know
// their own bound can tighten it here; tests use it to keep the deadline path
// fast.
func (r *Registry) SetTimeout(kind string, d time.Duration) { r.timeouts[kind] = d }

func (r *Registry) timeoutFor(kind string) time.Duration {
	if d, ok := r.timeouts[kind]; ok && d > 0 {
		return d
	}
	return DefaultOpTimeout
}

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
		//
		// The skip carries WHICH prerequisite broke and ITS error (SIGMA-301).
		// This matters far beyond tidiness: most of a deploy pipeline's ops
		// (image.pull, volume.ensure, network.ensure) map to no deploy phase in
		// the CP, so the only op whose status the operator ever sees is the
		// dependent rollout — and a bare "prerequisite failed" turned a registry
		// 401, a volume-name collision or a full disk into a deploy that failed
		// for no stated reason, recoverable only by SSHing to the host and
		// reading the agent journal. Naming the dep and quoting its error fixes
		// the message for every op kind at once, including the ops the CP has no
		// other route for.
		skip := false
		skipErr := "prerequisite failed"
		for _, dep := range op.DependsOn {
			if failed[dep] {
				skip = true
				if prev := results[dep]; prev.Err != "" {
					skipErr = fmt.Sprintf("prerequisite %s failed: %s", dep, prev.Err)
				} else {
					skipErr = fmt.Sprintf("prerequisite %s failed", dep)
				}
				break
			}
		}
		if skip {
			res := OpResult{Version: doc.Version, OpID: op.ID, State: "skipped", Err: skipErr, At: time.Now()}
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
		} else {
			// Every op runs under its own deadline (SIGMA-339) so a handler that
			// never returns fails loudly instead of wedging this serial loop for
			// the life of the process.
			budget := r.timeoutFor(op.Kind)
			opCtx, cancel := context.WithTimeout(ctx, budget)
			err := h(opCtx, op)
			timedOut := opCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil
			cancel()
			switch {
			case timedOut:
				// Cut off mid-flight: whatever the handler returned (even nil) the
				// op did not complete on its own terms, so it is a failure. Say so
				// in the journal — this is the line an operator reads when the
				// deploy view shows the op red.
				res.State = "failed"
				res.Err = fmt.Sprintf("op timed out after %s", budget)
				if err != nil {
					res.Err += ": " + err.Error()
				}
			case err != nil:
				res.State = "failed"
				res.Err = err.Error()
			}
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
