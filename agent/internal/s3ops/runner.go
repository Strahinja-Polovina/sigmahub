package s3ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/agent/internal/apply"
	"github.com/Strahinja-Polovina/sigmahub/agent/internal/dsd"
)

// KindS3Configure is the on-demand bucket/key/quota op kind. Byte-identical with
// the CP dsd package.
const KindS3Configure = "s3.configure"

// opTimeout bounds one s3.configure execution.
const opTimeout = 5 * time.Minute

// ErrOpSettled marks an op the CP no longer considers open (already applied) —
// the handler becomes a no-op, mirroring the backup runner's ErrRunSettled.
var ErrOpSettled = errors.New("s3 op already settled")

// OpSpec is the wire payload of an s3.configure op (identifiers only — no
// secret rides the DSD; the agent fetches credentials per-op).
type OpSpec struct {
	OpID       string `json:"opId"`
	ResourceID string `json:"resourceId"`
	Engine     string `json:"engine"`
	Container  string `json:"container"`
	Endpoint   string `json:"endpoint"`
	Action     string `json:"action"` // create-bucket|delete-bucket|set-quota|create-key|measure
	Bucket     string `json:"bucket"`
	AccessKey  string `json:"accessKey"`  // create-key: the CP-generated per-bucket key id
	QuotaBytes int64  `json:"quotaBytes"` // set-quota
}

// OpCredential is the per-op material fetched from the CP (audited release): the
// root credential to authenticate, plus the new per-bucket secret for create-key.
type OpCredential struct {
	RootAccessKey string `json:"rootAccessKey"`
	RootSecretKey string `json:"rootSecretKey"`
	NewSecretKey  string `json:"newSecretKey,omitempty"`
}

// CredentialFetcher resolves one op's credential from the control plane.
type CredentialFetcher func(ctx context.Context, opID string) (OpCredential, error)

// Reporter posts an op's terminal outcome (+ measured bytes for measure ops).
type Reporter func(ctx context.Context, opID string, ok bool, detail string, measuredBytes int64)

// Runner owns the s3.configure handler.
type Runner struct {
	http   *http.Client
	exec   Execer
	creds  CredentialFetcher
	report Reporter
	log    *slog.Logger
}

// NewRunner builds the runner. hc dials the mesh endpoint; exec runs weed shell
// inside the SeaweedFS container.
func NewRunner(hc *http.Client, exec Execer, creds CredentialFetcher, report Reporter, log *slog.Logger) *Runner {
	return &Runner{http: hc, exec: exec, creds: creds, report: report, log: log}
}

// Register wires the op kind into the apply registry.
func (r *Runner) Register(reg *apply.Registry) {
	reg.Register(KindS3Configure, r.opConfigure)
}

func (r *Runner) fail(ctx context.Context, opID string, err error) error {
	r.report(ctx, opID, false, err.Error(), 0)
	return err
}

// opConfigure dispatches one bucket/key/quota/measure action.
func (r *Runner) opConfigure(ctx context.Context, op dsd.Op) error {
	var spec OpSpec
	if err := json.Unmarshal(op.Spec, &spec); err != nil {
		return fmt.Errorf("decode s3 op spec: %w", err)
	}
	if spec.OpID == "" || spec.Action == "" {
		return fmt.Errorf("s3 op spec missing opId/action")
	}
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	cred, err := r.creds(ctx, spec.OpID)
	if errors.Is(err, ErrOpSettled) {
		r.log.Info("s3 op already settled; skipping", "op", spec.OpID)
		return nil
	}
	if err != nil {
		return fmt.Errorf("fetch s3 op credential: %w", err)
	}
	s3 := NewS3Client(r.http, spec.Endpoint, cred.RootAccessKey, cred.RootSecretKey)

	switch spec.Action {
	case "create-bucket":
		if err := s3.CreateBucket(ctx, spec.Bucket); err != nil {
			return r.fail(ctx, spec.OpID, err)
		}
		r.report(ctx, spec.OpID, true, "bucket created", 0)
	case "delete-bucket":
		if err := s3.DeleteBucket(ctx, spec.Bucket); err != nil {
			return r.fail(ctx, spec.OpID, err)
		}
		r.report(ctx, spec.OpID, true, "bucket deleted", 0)
	case "set-quota":
		if err := r.setBucketQuota(ctx, spec, cred); err != nil {
			return r.fail(ctx, spec.OpID, err)
		}
		r.report(ctx, spec.OpID, true, "quota set", 0)
	case "create-key":
		if err := r.createBucketKey(ctx, spec, cred); err != nil {
			return r.fail(ctx, spec.OpID, err)
		}
		r.report(ctx, spec.OpID, true, "per-bucket key created", 0)
	case "measure":
		bytes, err := s3.MeasureBucket(ctx, spec.Bucket)
		if err != nil {
			return r.fail(ctx, spec.OpID, err)
		}
		r.report(ctx, spec.OpID, true, "measured", bytes)
	default:
		return r.fail(ctx, spec.OpID, fmt.Errorf("unknown s3 action %q", spec.Action))
	}
	r.log.Info("s3 op complete", "op", spec.OpID, "action", spec.Action)
	return nil
}
