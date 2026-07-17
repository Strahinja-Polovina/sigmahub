package backup

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// walFakeDocker scripts the three exec calls a WAL cycle makes: list (ls),
// bundle (tar), cleanup (rm). It records them so the test can assert the
// ship-before-delete ordering and the exact segments.
type walFakeDocker struct {
	segments []string // what `ls` returns
	tarData  []byte
	calls    [][]string
}

func (f *walFakeDocker) ContainerExec(_ context.Context, _ string, cmd []string, out io.Writer) (int, string, error) {
	f.calls = append(f.calls, cmd)
	joined := strings.Join(cmd, " ")
	switch {
	case strings.HasPrefix(joined, "sh -c ls"):
		_, _ = out.Write([]byte(strings.Join(f.segments, "\n") + "\n"))
	case cmd[0] == "tar":
		_, _ = out.Write(f.tarData)
	}
	return 0, "", nil
}

// The rest of the Docker interface is unused by the shipper.
func (f *walFakeDocker) PutArchive(context.Context, string, string, io.Reader) error { return nil }
func (f *walFakeDocker) ContainerCreate(context.Context, string, any) (string, error) {
	return "", nil
}
func (f *walFakeDocker) ContainerStart(context.Context, string) error  { return nil }
func (f *walFakeDocker) ContainerRemove(context.Context, string, bool) error { return nil }
func (f *walFakeDocker) ImagePull(context.Context, string) error       { return nil }

func TestWALShipperShipsThenDeletes(t *testing.T) {
	dir := t.TempDir()
	stubRestic(t, dir)

	fd := &walFakeDocker{
		segments: []string{"000000010000000000000003", "000000010000000000000002.tmp", "000000010000000000000001"},
		tarData:  []byte("tar payload"),
	}
	var reportedSeg string
	s := NewWALShipper(fd,
		func(context.Context) ([]string, error) { return []string{"res_pg"}, nil },
		func(context.Context, string) (Credential, error) {
			return Credential{Repository: "s3:s3.example/bucket/sigmahub/res_pg", RepoKey: "k", AccessKey: "a", SecretKey: "s"}, nil
		},
		func(_ context.Context, _, seg string) { reportedSeg = seg },
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		time.Minute,
	)
	s.now = func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }

	if err := s.shipOne(context.Background(), "res_pg"); err != nil {
		t.Fatalf("shipOne: %v", err)
	}

	// Three exec calls in order: ls, tar, rm.
	if len(fd.calls) != 3 {
		t.Fatalf("exec calls = %d, want 3 (%v)", len(fd.calls), fd.calls)
	}
	if fd.calls[1][0] != "tar" || fd.calls[2][0] != "rm" {
		t.Fatalf("call order = %v", fd.calls)
	}
	// The .tmp segment (half-written) must never be tarred or deleted.
	tarJoined := strings.Join(fd.calls[1], " ")
	rmJoined := strings.Join(fd.calls[2], " ")
	if strings.Contains(tarJoined, ".tmp") || strings.Contains(rmJoined, ".tmp") {
		t.Fatalf("in-progress .tmp segment must be skipped: tar=%q rm=%q", tarJoined, rmJoined)
	}
	// tar takes bare segment names (relative to -C spool); rm takes full paths.
	if !strings.Contains(tarJoined, "000000010000000000000001 000000010000000000000003") {
		t.Fatalf("tar must carry both ready segments sorted: %q", tarJoined)
	}
	if !strings.Contains(rmJoined, WALSpoolDir+"/000000010000000000000003") {
		t.Fatalf("rm must use full spool paths: %q", rmJoined)
	}
	// High-water mark is the newest shipped segment.
	if reportedSeg != "000000010000000000000003" {
		t.Fatalf("reported segment = %q", reportedSeg)
	}
}

func TestWALShipperNoSegmentsIsNoOp(t *testing.T) {
	dir := t.TempDir()
	stubRestic(t, dir)
	fd := &walFakeDocker{segments: nil}
	credCalled := false
	s := NewWALShipper(fd,
		func(context.Context) ([]string, error) { return []string{"res_pg"}, nil },
		func(context.Context, string) (Credential, error) {
			credCalled = true
			return Credential{}, nil
		},
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		time.Minute,
	)
	if err := s.shipOne(context.Background(), "res_pg"); err != nil {
		t.Fatal(err)
	}
	// Empty spool: only the ls probe runs; no credential fetch, no tar/rm.
	if credCalled {
		t.Fatal("no segments must not fetch a credential")
	}
	if len(fd.calls) != 1 {
		t.Fatalf("exec calls = %d, want 1 (ls only)", len(fd.calls))
	}
}
