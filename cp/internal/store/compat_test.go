package store

import (
	"strings"
	"testing"
	"time"
)

// The rule that keeps a fleet online: a requirement whose fact is absent is
// UNEVALUATED, never violated.
//
// Stated per requirement rather than once with an empty HostFacts, because the
// dangerous version of this bug is partial — an agent that reports its arch but
// not its GPU is the normal state during a rollout, and a checker that treated
// the fields it happened to receive as the whole truth would refuse it for the
// ones it did not.
func TestAbsentFactCannotFailARequirement(t *testing.T) {
	// A host that satisfies `gpu` — the type with the most requirements — so
	// each case below can blank exactly one fact and nothing else fires.
	compatible := HostFacts{
		Arch: "amd64", Distro: "ubuntu-24.04", DiskTotalBytes: 2 << 40,
		GPU: &GPUInventory{Vendor: "nvidia", Count: 1, DriverVersion: "550.54.15"},
	}
	if fails := CheckServerCompatibility("gpu", compatible); len(fails) != 0 {
		t.Fatalf("the fixture itself is not compatible: %+v", fails)
	}

	for _, tc := range []struct {
		name  string
		blank func(HostFacts) HostFacts
	}{
		{"no distro reported", func(f HostFacts) HostFacts { f.Distro = ""; return f }},
		{"no arch reported", func(f HostFacts) HostFacts { f.Arch = ""; return f }},
		{"no disk reported", func(f HostFacts) HostFacts { f.DiskTotalBytes = 0; return f }},
		{"never looked for a GPU", func(f HostFacts) HostFacts { f.GPU = nil; return f }},
		{"reports nothing at all", func(HostFacts) HostFacts { return HostFacts{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, typ := range ServerTypes() {
				if fails := CheckServerCompatibility(typ, tc.blank(compatible)); len(fails) != 0 {
					t.Errorf("%s: %+v — an unreported fact must not refuse a host", typ, fails)
				}
			}
		})
	}

	// The distinction that makes the GPU check work at all: nil is "never
	// looked", an empty inventory is "looked, found nothing".
	looked := compatible
	looked.GPU = &GPUInventory{}
	if fails := CheckServerCompatibility("gpu", looked); len(fails) != 1 || fails[0].ID != ReqGPU {
		t.Fatalf("an explicit empty inventory must fail the GPU requirement, got %+v", fails)
	}
}

// A type the catalog does not know cannot be judged, so it is not refused: the
// only rows that can hold one are legacy hosts enrolled under a type since
// removed, and marking them incompatible would attach a reason no requirement
// backs.
func TestUnknownServerTypeIsNotJudged(t *testing.T) {
	if fails := CheckServerCompatibility("toaster", HostFacts{Distro: "fedora-41", Arch: "s390x"}); fails != nil {
		t.Fatalf("unknown type produced %+v", fails)
	}
}

// The status machine, stated as a table. Every cell here is a product decision
// that a one-word edit could reverse.
func TestCompatibilityStatusTransitions(t *testing.T) {
	fail := []FailedRequirement{{ID: ReqGPU}}
	for _, tc := range []struct {
		name           string
		prev           string
		fails          []FailedRequirement
		agentCheckedIn bool
		want           string
	}{
		{"refused at registration", ServerStatusProvisioning, fail, false, ServerStatusIncompatible},
		{"refused on a heartbeat", ServerStatusRunning, fail, true, ServerStatusIncompatible},
		{"still refused", ServerStatusIncompatible, fail, true, ServerStatusIncompatible},
		{"first heartbeat", ServerStatusProvisioning, nil, true, ServerStatusRunning},
		{"recovers on a later heartbeat", ServerStatusIncompatible, nil, true, ServerStatusRunning},
		{"agent comes back", ServerStatusUnreachable, nil, true, ServerStatusRunning},
		// Registration is not evidence of liveness — the heartbeat that follows
		// seconds later is, and always has been.
		{"clean registration waits for a heartbeat", ServerStatusProvisioning, nil, false, ServerStatusProvisioning},
		{"cleared before the agent ever checked in", ServerStatusIncompatible, nil, false, ServerStatusProvisioning},
		// A silent server stays silent: clearing an incompatibility must not
		// resurrect a host the sweeper has already given up on.
		{"unreachable stays unreachable", ServerStatusUnreachable, nil, false, ServerStatusUnreachable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := compatibilityStatus(tc.prev, tc.fails, tc.agentCheckedIn); got != tc.want {
				t.Fatalf("compatibilityStatus(%q, %d fails, checkedIn=%v) = %q, want %q",
					tc.prev, len(tc.fails), tc.agentCheckedIn, got, tc.want)
			}
		})
	}
}

// A type change is evidence of nothing about the machine.
//
// The bug this pins: SetServerType passed `last_seen_at IS NOT NULL` as "the
// agent checked in", conflating "has ever spoken to us" with "is speaking to us
// now". Re-filing the type of a host the sweeper had already given up on wrote
// `running` over `unreachable` — the server became billable without a single
// heartbeat, and the sweeper flipped it back on its next tick with a second
// spurious unreachable alert for a machine that never came back.
func TestTypeChangeNeverInventsLiveness(t *testing.T) {
	recent := time.Now().Add(-5 * time.Second)
	stale := time.Now().Add(-10 * time.Minute)
	gpuFail := []FailedRequirement{{ID: ReqGPU, Reason: "no GPU"}}

	for _, tc := range []struct {
		name     string
		prev     string
		fails    []FailedRequirement
		lastSeen *time.Time
		want     string
	}{
		// The defect, stated directly.
		{"an unreachable host is not revived by a re-file", ServerStatusUnreachable, nil, &stale, ServerStatusUnreachable},
		{"nor by one that clears an incompatibility", ServerStatusIncompatible, nil, &stale, ServerStatusUnreachable},
		// Clearing the flag on a host that IS alive gives it back.
		{"a live host comes back to running", ServerStatusIncompatible, nil, &recent, ServerStatusRunning},
		// Never seen at all: it is still waiting for its first heartbeat.
		{"a host that never checked in stays provisioning", ServerStatusIncompatible, nil, nil, ServerStatusProvisioning},
		// A re-file must not promote a host that is merely waiting.
		{"provisioning is not promoted", ServerStatusProvisioning, nil, &recent, ServerStatusProvisioning},
		// Liveness is untouched when there is no flag to clear.
		{"a running host stays running", ServerStatusRunning, nil, &recent, ServerStatusRunning},
		// And a failing gate always wins, whatever the liveness.
		{"a failing gate marks incompatible", ServerStatusRunning, gpuFail, &recent, ServerStatusIncompatible},
		{"even on an unreachable host", ServerStatusUnreachable, gpuFail, &stale, ServerStatusIncompatible},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := statusAfterTypeChange(tc.prev, tc.fails, tc.lastSeen, DefaultStaleAfter)
			if got != tc.want {
				t.Fatalf("statusAfterTypeChange(%q, %d fails, lastSeen=%v) = %q, want %q",
					tc.prev, len(tc.fails), tc.lastSeen != nil, got, tc.want)
			}
		})
	}
}

// The reason is rendered verbatim, and its inputs are strings the host chose.
func TestReasonTextIsBounded(t *testing.T) {
	long := strings.Repeat("x", 4000)
	fails := CheckServerCompatibility("gpu", HostFacts{
		Distro: "fedora-41", DistroName: long, Arch: "amd64",
	})
	if len(fails) == 0 {
		t.Fatal("expected an ordinary box to fail the gpu gate")
	}
	for _, f := range fails {
		if len(f.Reason) > 400 {
			t.Errorf("reason for %q is %d bytes — a refusal that scrolls is a refusal nobody reads", f.ID, len(f.Reason))
		}
		if len(f.Detected) > maxFactText+4 {
			t.Errorf("detected for %q is %d bytes", f.ID, len(f.Detected))
		}
	}
}
