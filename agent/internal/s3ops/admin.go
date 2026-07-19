package s3ops

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Engine-specific admin: per-bucket access keys (least privilege) and per-bucket
// quotas are NOT part of the plain S3 API, so they dispatch per engine.
//
//   - SeaweedFS ships the `weed` binary, so its admin runs through `weed shell`
//     (`s3.configure`, `s3.bucket.quota`) via docker exec — no stdin needed
//     (the command is echo-piped through `sh -c`).
//   - MinIO's server image does NOT ship `mc`, so its admin runs over the MinIO
//     Admin API (SigV4-signed HTTP). The pinned release may require an encrypted
//     request body for add-service-account; that exact wire form is finalized on
//     staging — the call is made faithfully and any rejection surfaces as an
//     honest failure rather than a silent no-op (never fabricated success).
//
// The commands are derived HERE from the engine (never the DSD), preserving the
// no-generic-run-shell invariant.

// Execer runs a command inside a container (the backup package's Docker slice).
// ContainerExecEnv additionally sets the exec's process environment so a secret
// can be handed to the command out of band of its argv (SIGMA-79).
type Execer interface {
	ContainerExec(ctx context.Context, containerID string, cmd []string, out io.Writer) (int, string, error)
	ContainerExecEnv(ctx context.Context, containerID string, cmd, env []string, out io.Writer) (int, string, error)
}

// weedShell runs one (non-secret) weed-shell command inside the SeaweedFS
// container. weed shell reads from stdin, so the command is piped in via
// `sh -c 'echo … | weed shell'`. The filer address is the in-container default.
func weedShell(ctx context.Context, ex Execer, container, command string) error {
	full := fmt.Sprintf("echo %s | weed shell -filer=localhost:8888", shQuote(command))
	code, tail, err := ex.ContainerExec(ctx, container, []string{"sh", "-c", full}, io.Discard)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("weed shell exited %d: %s", code, tail)
	}
	return nil
}

// weedShellSecret runs a weed-shell command whose secret is supplied via the
// exec environment as $SK, never via argv. `command` must reference the secret
// as the literal token $SK; the container shell expands it from the env at run
// time, so the secret stays out of the process cmdline (ps / /proc/*/cmdline).
// The command is double-quoted so $SK expands; every other field it carries is
// a CP-generated identifier (access key id, validated bucket name), never
// free-form input (SIGMA-79).
func weedShellSecret(ctx context.Context, ex Execer, container, command, secret string) error {
	full := fmt.Sprintf(`echo "%s" | weed shell -filer=localhost:8888`, command)
	code, tail, err := ex.ContainerExecEnv(ctx, container, []string{"sh", "-c", full}, []string{"SK=" + secret}, io.Discard)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("weed shell exited %d: %s", code, tail)
	}
	return nil
}

// shQuote single-quotes a string for safe embedding in a `sh -c` command.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// CreateBucketKey provisions a least-privilege access key scoped to one bucket.
func (r *Runner) createBucketKey(ctx context.Context, spec OpSpec, cred OpCredential) error {
	switch spec.Engine {
	case "seaweedfs":
		// $SK is expanded from the exec env by weedShellSecret — the secret is
		// never interpolated into argv (SIGMA-79).
		cmd := fmt.Sprintf("s3.configure -user %s -access_key %s -secret_key $SK -buckets %s -actions Read,Write -apply",
			spec.AccessKey, spec.AccessKey, spec.Bucket)
		return weedShellSecret(ctx, r.exec, spec.Container, cmd, cred.NewSecretKey)
	case "minio":
		return r.minioAddServiceAccount(ctx, spec, cred)
	}
	return fmt.Errorf("unsupported engine %q", spec.Engine)
}

// SetBucketQuota enforces a per-bucket size cap.
func (r *Runner) setBucketQuota(ctx context.Context, spec OpSpec, cred OpCredential) error {
	switch spec.Engine {
	case "seaweedfs":
		cmd := fmt.Sprintf("s3.bucket.quota -name %s -size %d", spec.Bucket, spec.QuotaBytes)
		return weedShell(ctx, r.exec, spec.Container, cmd)
	case "minio":
		return r.minioSetBucketQuota(ctx, spec, cred)
	}
	return fmt.Errorf("unsupported engine %q", spec.Engine)
}

// minioSetBucketQuota calls the MinIO Admin API set-bucket-quota (quota rides
// query params). Staging-validated against the pinned release.
func (r *Runner) minioSetBucketQuota(ctx context.Context, spec OpSpec, cred OpCredential) error {
	q := fmt.Sprintf("bucket=%s&quota=%d&unit=B", spec.Bucket, spec.QuotaBytes)
	return r.minioAdmin(ctx, spec, cred, http.MethodPut, "/minio/admin/v3/set-bucket-quota", q, nil)
}

// minioAddServiceAccount calls the MinIO Admin API add-service-account. The
// pinned release may require an encrypted body; that wire form is finalized on
// staging (an unencrypted-body rejection surfaces as an honest failure).
func (r *Runner) minioAddServiceAccount(ctx context.Context, spec OpSpec, cred OpCredential) error {
	body := fmt.Sprintf(`{"accessKey":%q,"secretKey":%q,"policy":%q}`,
		spec.AccessKey, cred.NewSecretKey, minioBucketPolicy(spec.Bucket))
	return r.minioAdmin(ctx, spec, cred, http.MethodPut, "/minio/admin/v3/add-service-account", "", []byte(body))
}

// minioBucketPolicy is a least-privilege IAM policy scoped to one bucket.
func minioBucketPolicy(bucket string) string {
	return fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:*"],"Resource":["arn:aws:s3:::%s","arn:aws:s3:::%s/*"]}]}`, bucket, bucket)
}

// minioAdmin issues a SigV4-signed MinIO Admin API request with the root
// credential over the mesh endpoint.
func (r *Runner) minioAdmin(ctx context.Context, spec OpSpec, cred OpCredential, method, path, rawQuery string, body []byte) error {
	u := strings.TrimSuffix(spec.Endpoint, "/") + path
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	var rdr io.Reader
	payloadHash := EmptyPayloadHash
	if body != nil {
		rdr = strings.NewReader(string(body))
		payloadHash = sha256hex(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return err
	}
	SignV4(req, cred.RootAccessKey, cred.RootSecretKey, s3Region, "s3", payloadHash, time.Now())
	resp, err := r.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return s3Error(resp)
	}
	return nil
}
