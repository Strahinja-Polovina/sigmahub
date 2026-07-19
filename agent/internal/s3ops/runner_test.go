package s3ops

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/agent/internal/dsd"
)

// fakeExec records weed-shell invocations (the SeaweedFS admin path). envs[i]
// holds the exec environment passed alongside calls[i] (nil for a plain
// ContainerExec).
type fakeExec struct {
	calls [][]string
	envs  [][]string
	fail  bool
}

func (f *fakeExec) ContainerExec(_ context.Context, _ string, cmd []string, _ io.Writer) (int, string, error) {
	f.calls = append(f.calls, cmd)
	f.envs = append(f.envs, nil)
	if f.fail {
		return 1, "boom", nil
	}
	return 0, "", nil
}

func (f *fakeExec) ContainerExecEnv(_ context.Context, _ string, cmd, env []string, _ io.Writer) (int, string, error) {
	f.calls = append(f.calls, cmd)
	f.envs = append(f.envs, env)
	if f.fail {
		return 1, "boom", nil
	}
	return 0, "", nil
}

type reported struct {
	ok     bool
	detail string
	bytes  int64
	count  int
}

func newRunner(t *testing.T, ex Execer, cred OpCredential) (*Runner, *reported) {
	t.Helper()
	rep := &reported{}
	r := NewRunner(http.DefaultClient, ex,
		func(context.Context, string) (OpCredential, error) { return cred, nil },
		func(_ context.Context, _ string, ok bool, detail string, b int64) {
			rep.ok, rep.detail, rep.bytes = ok, detail, b
			rep.count++
		},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return r, rep
}

func op(t *testing.T, spec OpSpec) dsd.Op {
	t.Helper()
	b, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	return dsd.Op{ID: "s3cfg:" + spec.OpID, Kind: KindS3Configure, Spec: b}
}

// TestCreateAndDeleteBucketOverS3API drives the engine-agnostic S3 path against a
// stub S3 endpoint, asserting the signed PUT/DELETE reach it and succeed.
func TestCreateAndDeleteBucketOverS3API(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r, rep := newRunner(t, &fakeExec{}, OpCredential{RootAccessKey: "sigma", RootSecretKey: "sk"})

	if err := r.opConfigure(context.Background(), op(t, OpSpec{
		OpID: "o1", Engine: "minio", Endpoint: srv.URL, Action: "create-bucket", Bucket: "media",
	})); err != nil {
		t.Fatalf("create: %v", err)
	}
	if gotMethod != http.MethodPut || gotPath != "/media" || !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256") {
		t.Fatalf("create req = %s %s auth=%q", gotMethod, gotPath, gotAuth)
	}
	if !rep.ok || rep.detail != "bucket created" {
		t.Fatalf("create report = %+v", rep)
	}

	if err := r.opConfigure(context.Background(), op(t, OpSpec{
		OpID: "o2", Engine: "minio", Endpoint: srv.URL, Action: "delete-bucket", Bucket: "media",
	})); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Fatalf("delete method = %s", gotMethod)
	}
}

// TestMeasureBucketSumsObjectSizes checks the paginated ListObjectsV2 measurement.
func TestMeasureBucketSumsObjectSizes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<ListBucketResult><Contents><Size>100</Size></Contents><Contents><Size>250</Size></Contents><IsTruncated>false</IsTruncated></ListBucketResult>`)
	}))
	defer srv.Close()

	r, rep := newRunner(t, &fakeExec{}, OpCredential{RootAccessKey: "sigma", RootSecretKey: "sk"})
	if err := r.opConfigure(context.Background(), op(t, OpSpec{
		OpID: "o3", Engine: "minio", Endpoint: srv.URL, Action: "measure", Bucket: "media",
	})); err != nil {
		t.Fatalf("measure: %v", err)
	}
	if !rep.ok || rep.bytes != 350 {
		t.Fatalf("measure report = %+v, want 350 bytes", rep)
	}
}

// TestSeaweedKeyUsesWeedShell asserts the SeaweedFS per-bucket key path execs a
// weed-shell s3.configure command carrying the scoped bucket + actions, and that
// the new secret rides the exec ENVIRONMENT (as $SK), never the process argv
// (SIGMA-79) — nor the DSD (it comes from the per-op credential).
func TestSeaweedKeyUsesWeedShell(t *testing.T) {
	ex := &fakeExec{}
	r, rep := newRunner(t, ex, OpCredential{RootAccessKey: "sigma", RootSecretKey: "sk", NewSecretKey: "newsecret"})
	if err := r.opConfigure(context.Background(), op(t, OpSpec{
		OpID: "o4", Engine: "seaweedfs", Container: "sigmahub-res_s3", Action: "create-key",
		Bucket: "media", AccessKey: "bk_abc",
	})); err != nil {
		t.Fatalf("create-key: %v", err)
	}
	if !rep.ok || len(ex.calls) != 1 {
		t.Fatalf("report=%+v calls=%v", rep, ex.calls)
	}
	joined := strings.Join(ex.calls[0], " ")
	for _, want := range []string{"weed shell", "s3.configure", "-buckets media", "-access_key bk_abc", "$SK", "Read,Write"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("weed shell cmd %q missing %q", joined, want)
		}
	}
	// The freshly minted secret must NOT be in argv (ps / /proc/cmdline) — it is
	// handed to the exec via the environment instead.
	if strings.Contains(joined, "newsecret") {
		t.Fatalf("secret leaked into argv: %q", joined)
	}
	if env := strings.Join(ex.envs[0], " "); !strings.Contains(env, "SK=newsecret") {
		t.Fatalf("secret not passed via exec env, got %q", env)
	}
}

// TestUnknownActionFails guards the dispatch.
func TestUnknownActionFails(t *testing.T) {
	r, rep := newRunner(t, &fakeExec{}, OpCredential{})
	if err := r.opConfigure(context.Background(), op(t, OpSpec{OpID: "o5", Action: "nope"})); err == nil {
		t.Fatal("unknown action must fail")
	}
	if rep.ok {
		t.Fatal("must report failure")
	}
}
