package reconciler

// SIGMA-65: on-demand S3 bucket/key/quota/measure ops render as typed
// s3.configure DSD ops. Each op carries identifiers ONLY — the root credential
// and the new per-bucket secret are fetched by the agent per-op through the
// audited /v1/agent/s3-op-credential path, so a captured DSD leaks nothing. The
// engine-specific admin command is derived by the agent from the engine name,
// never carried here (the no-generic-run-shell invariant).

import (
	"encoding/json"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// s3ConfigureSpec is the wire payload of an s3.configure op. Field tags are
// byte-identical with the agent's s3ops.OpSpec — DO NOT rename.
type s3ConfigureSpec struct {
	OpID       string `json:"opId"`
	ResourceID string `json:"resourceId"`
	Engine     string `json:"engine"`
	Container  string `json:"container"`
	Endpoint   string `json:"endpoint"`
	Action     string `json:"action"`
	Bucket     string `json:"bucket"`
	AccessKey  string `json:"accessKey"`
	QuotaBytes int64  `json:"quotaBytes"`
}

// renderS3ConfigureOps renders a server's open s3 ops. Each op is one DSD op
// with id "s3cfg:<opId>" so status ingest and the dedicated result report map
// back to the pending_s3_ops row.
func renderS3ConfigureOps(ops []store.S3OpSpec) []dsd.Op {
	out := make([]dsd.Op, 0, len(ops))
	for _, o := range ops {
		spec := s3ConfigureSpec{
			OpID:       o.OpID,
			ResourceID: o.ResourceID,
			Engine:     o.Engine,
			Container:  o.Container,
			Endpoint:   o.Endpoint,
			Action:     o.Action,
			Bucket:     o.Bucket,
			AccessKey:  o.AccessKey,
			QuotaBytes: o.QuotaBytes,
		}
		b, _ := json.Marshal(spec)
		out = append(out, dsd.Op{ID: "s3cfg:" + o.OpID, Kind: dsd.KindS3Configure, Spec: b})
	}
	return out
}
