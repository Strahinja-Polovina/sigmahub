package uninstall

// These tests are about ORDER, because order is the whole defect class here.
// Each individual step is a two-line call into Docker, systemd or os.Remove;
// what makes a decommission work or hang is which of them runs before the
// acknowledgement that tells the control plane the machine is done — and a
// suite that only exercised the steps would stay green through a reordering
// that leaves every operator's disconnect timing out.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/agent/internal/apply"
	"github.com/Strahinja-Polovina/sigmahub/agent/internal/dsd"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// recorder builds a Steps whose every action appends its name to one slice, so
// a test can assert the sequence rather than the set.
type recorder struct {
	calls []string
	// fail maps a step name to the error it returns.
	fail map[string]error
	// ackOK / ackDetail capture what was reported.
	ackOK     bool
	ackDetail string
	ackCalled bool
	exited    bool
	// channelBroken is set by the steps that destroy the path the ack travels
	// on. Ack consults it, so "the ack ran too late" fails as a real failure
	// rather than as an index comparison a refactor could quietly invert.
	channelBroken string
}

func newRecorder() *recorder { return &recorder{fail: map[string]error{}} }

func (r *recorder) step(name string) func(context.Context) error {
	return func(context.Context) error {
		r.calls = append(r.calls, name)
		return r.fail[name]
	}
}

func (r *recorder) steps() Steps {
	return Steps{
		RemoveContainers: r.step("containers"),
		RemoveNetworks:   r.step("networks"),
		RemoveVolumes:    r.step("volumes"),
		Ack: func(_ context.Context, ok bool, detail string) error {
			r.calls = append(r.calls, "ack")
			r.ackCalled, r.ackOK, r.ackDetail = true, ok, detail
			if r.channelBroken != "" {
				// The real failure mode: the token file is gone, or the route
				// the control plane sits behind was just torn out.
				return errors.New("ack channel already destroyed by " + r.channelBroken)
			}
			return r.fail["ack"]
		},
		TearDownMesh: func(context.Context, string) error {
			r.calls = append(r.calls, "mesh")
			r.channelBroken = "the WireGuard teardown"
			return r.fail["mesh"]
		},
		RemoveUnit: r.step("unit"),
		RemoveDataDir: func(context.Context) error {
			r.calls = append(r.calls, "dataDir")
			r.channelBroken = "the data-dir removal (the agent token lives there)"
			return r.fail["dataDir"]
		},
		RemoveBinary: r.step("binary"),
		Exit:         func() { r.exited = true; r.calls = append(r.calls, "exit") },
	}
}

func (r *recorder) index(name string) int {
	for i, c := range r.calls {
		if c == name {
			return i
		}
	}
	return -1
}

func op(t *testing.T, spec Spec) dsd.Op {
	t.Helper()
	b, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	return dsd.Op{ID: "agent:uninstall:srv_1", Kind: Kind, Spec: b}
}

func TestTeardownOrder(t *testing.T) {
	r := newRecorder()
	u := &Uninstaller{Log: quietLog(), ServerID: "srv_1", Steps: r.steps()}

	if err := u.Handle(context.Background(), op(t, Spec{ServerID: "srv_1", MeshInterface: "sigma0"})); err != nil {
		t.Fatalf("handle: %v", err)
	}

	want := []string{"containers", "networks", "ack", "mesh", "unit", "dataDir", "binary", "exit"}
	if strings.Join(r.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("teardown order =\n  %v\nwant\n  %v", r.calls, want)
	}
	if !r.ackOK || r.ackDetail != "" {
		t.Fatalf("clean teardown acked ok=%v detail=%q, want a clean ack", r.ackOK, r.ackDetail)
	}
}

// The property the ordering exists for, expressed as the failure it prevents:
// both the WireGuard teardown and the data-dir removal destroy the channel the
// ack needs, so an ack sequenced after either of them cannot land.
func TestAckPrecedesTeardownOfWhatTheAckNeeds(t *testing.T) {
	r := newRecorder()
	u := &Uninstaller{Log: quietLog(), ServerID: "srv_1", Steps: r.steps()}
	if err := u.Handle(context.Background(), op(t, Spec{ServerID: "srv_1"})); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if !r.ackCalled {
		t.Fatal("the control plane was never acked; it would wait out the full decommission timeout")
	}
	ack := r.index("ack")
	for _, destructive := range []string{"mesh", "dataDir"} {
		if at := r.index(destructive); at >= 0 && at < ack {
			t.Fatalf("%q ran at %d, before the ack at %d — it destroys what the ack travels on",
				destructive, at, ack)
		}
	}
	// The workloads ARE the thing the control plane is waiting to hear about,
	// so they must be finished before we claim they are.
	for _, work := range []string{"containers", "networks"} {
		if at := r.index(work); at < 0 || at > ack {
			t.Fatalf("%q ran at %d, after the ack at %d — the ack would be claiming work that had not happened",
				work, at, ack)
		}
	}
}

// The default path leaves the customer's data alone; the opt-in destroys it,
// and only after the containers holding it are gone.
func TestVolumesOnlyOnExplicitOptIn(t *testing.T) {
	r := newRecorder()
	u := &Uninstaller{Log: quietLog(), ServerID: "srv_1", Steps: r.steps()}
	if err := u.Handle(context.Background(), op(t, Spec{ServerID: "srv_1"})); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if r.index("volumes") >= 0 {
		t.Fatal("named volumes were destroyed without the operator opting in")
	}

	r = newRecorder()
	u = &Uninstaller{Log: quietLog(), ServerID: "srv_1", Steps: r.steps()}
	if err := u.Handle(context.Background(), op(t, Spec{ServerID: "srv_1", PurgeVolumes: true})); err != nil {
		t.Fatalf("handle: %v", err)
	}
	vols := r.index("volumes")
	if vols < 0 {
		t.Fatal("purgeVolumes was set and no volume was removed")
	}
	if vols < r.index("containers") {
		t.Fatal("volumes were removed before the containers holding them")
	}
	if vols > r.index("ack") {
		t.Fatal("volumes were removed after the ack, so the control plane was told a half-truth")
	}
}

// A step that fails must not swallow the report. This is the case that used to
// leave the control plane waiting on a machine that had already given up.
func TestFailurePartwayStillReports(t *testing.T) {
	r := newRecorder()
	r.fail["containers"] = errors.New("docker daemon not reachable")
	u := &Uninstaller{Log: quietLog(), ServerID: "srv_1", Steps: r.steps()}

	err := u.Handle(context.Background(), op(t, Spec{ServerID: "srv_1"}))
	if err == nil {
		t.Fatal("a failed teardown reported success to the journal")
	}
	if !r.ackCalled {
		t.Fatal("a failed teardown never acked — the control plane hangs until its timeout")
	}
	if r.ackOK {
		t.Fatal("the ack claimed success after a step failed")
	}
	if !strings.Contains(r.ackDetail, "docker daemon not reachable") {
		t.Fatalf("ack detail %q does not say what failed, so the operator cannot know to clean up by hand", r.ackDetail)
	}
	// Everything after the failure still runs: stopping here would leave the
	// agent installed with a credential the control plane is about to revoke,
	// which is the original defect wearing a different hat.
	for _, step := range []string{"networks", "mesh", "unit", "dataDir", "binary"} {
		if r.index(step) < 0 {
			t.Errorf("step %q was skipped after an earlier failure", step)
		}
	}
	if !r.exited {
		t.Fatal("the process never exited")
	}
}

// And the mirror image: the ack itself fails (the CP is down, or a proxy ate
// it). The host teardown is already half-done and irreversible, so it must
// finish and exit rather than hang — the CP's timeout is the designed catch.
func TestAckFailureDoesNotStopTheTeardown(t *testing.T) {
	r := newRecorder()
	r.fail["ack"] = errors.New("503 from control plane")
	u := &Uninstaller{Log: quietLog(), ServerID: "srv_1", Steps: r.steps()}

	err := u.Handle(context.Background(), op(t, Spec{ServerID: "srv_1"}))
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("handle err = %v, want the ack failure surfaced", err)
	}
	for _, step := range []string{"mesh", "unit", "dataDir", "binary"} {
		if r.index(step) < 0 {
			t.Errorf("step %q was skipped because the ack failed", step)
		}
	}
	if !r.exited {
		t.Fatal("the agent kept running after a decommission it could not report")
	}
}

// The one op that ends a host refuses an identity that is not ours.
func TestForeignServerIDIsRefusedBeforeAnythingIsDestroyed(t *testing.T) {
	r := newRecorder()
	u := &Uninstaller{Log: quietLog(), ServerID: "srv_1", Steps: r.steps()}

	err := u.Handle(context.Background(), op(t, Spec{ServerID: "srv_other"}))
	if err == nil {
		t.Fatal("an uninstall op addressed to another server was applied")
	}
	if len(r.calls) != 0 {
		t.Fatalf("steps ran for a foreign op: %v", r.calls)
	}
}

// The op reaches the handler through the same registry as every other kind —
// the enforcement point for "no generic run-shell". A kind registered nowhere
// is rejected by Apply, so this is what makes the capability exist at all.
func TestRegisteredKindIsReachableThroughTheApplyRegistry(t *testing.T) {
	r := newRecorder()
	u := &Uninstaller{Log: quietLog(), ServerID: "srv_1", Steps: r.steps()}
	reg := apply.NewRegistry()
	u.Register(reg)

	j, err := apply.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()

	doc := dsd.Document{Version: 7, ServerID: "srv_1", Ops: []dsd.Op{op(t, Spec{ServerID: "srv_1"})}}
	results, err := reg.Apply(context.Background(), quietLog(), j, doc)
	if err != nil {
		t.Fatal(err)
	}
	res := results["agent:uninstall:srv_1"]
	if res.State != "applied" {
		t.Fatalf("op state = %q (%s), want applied", res.State, res.Err)
	}
	if !r.ackCalled || !r.exited {
		t.Fatalf("dispatch through the registry did not run the handler (ack=%v exit=%v)", r.ackCalled, r.exited)
	}
}
