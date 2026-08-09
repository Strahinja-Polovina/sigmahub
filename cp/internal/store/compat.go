package store

// The registration compatibility gate (SIGMA-203).
//
// A host used to be enrolled as whatever type the operator picked in the
// connect dialog, and nothing ever compared that choice against the machine.
// The failure showed up later and somewhere else: a box with no accelerator
// enrolled as `gpu`, billed at GPU weight from its first heartbeat, and the
// operator found out at the first model deploy, when the container refused to
// start. A 200 GB VPS enrolled as `storage` and the S3 bucket it existed to
// serve filled the disk.
//
// The catalog already carried the answer as DATA — ServerRequirements per type,
// each Requirement naming the agent fact it reads (server_catalog.go). This
// file is the walk over that data. It deliberately holds no per-type knowledge:
// adding a requirement is a field in the catalog plus a case here, never a
// branch in the registration handler.
//
// Two rules make the difference between a gate and an outage:
//
//  1. A requirement whose fact is ABSENT cannot fail. Absent is unknown, not
//     violated. The fleet upgrades over hours, so on the day this ships most
//     agents have never reported a distro, a disk size or a GPU inventory — and
//     failing them for it would mark the entire fleet incompatible on a
//     rollout, which is a far worse outage than the misfiled server type this
//     gate exists to catch. store.ParseHostFacts already returns "unknown" as
//     the zero value for exactly this reason.
//
//  2. The verdict is RE-EVALUATED on every heartbeat, not frozen at
//     registration. A driver gets installed, a disk gets grown, an agent
//     finally learns to report a fact — all of those must clear the state
//     without the operator re-enrolling the host. Recovery is the same code
//     path as the original refusal, run against fresher facts.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Server status vocabulary. Three of these predate this file and are spelled
// out as SQL literals all over the store; they are named here so the fourth —
// which is new, and which callers must distinguish from both `running` and
// `provisioning` — is not a bare string sprinkled across three packages.
//
// `incompatible` is deliberately NOT a flavour of provisioning: the host
// finished provisioning perfectly, its agent is installed, authenticated and
// heartbeating. What is wrong is the TYPE it was enrolled as, and the only
// exits are changing that type or disconnecting. Leaving it in `provisioning`
// would have shown a spinner forever for a state that will never resolve on
// its own, and marking it `running` would bill it and schedule work onto it.
//
// `decommissioning` (SIGMA-204) is the fifth, and the only one that is
// TERMINAL: every other status can be re-derived from what the host reports,
// this one is a decision the operator made about the machine's future and no
// fact the agent sends can revise it.
const (
	ServerStatusProvisioning    = "provisioning"
	ServerStatusRunning         = "running"
	ServerStatusUnreachable     = "unreachable"
	ServerStatusIncompatible    = "incompatible"
	ServerStatusDecommissioning = "decommissioning"
)

// FailedRequirement is one precondition the host did not meet, in the shape the
// dashboard renders. Machine-readable and human-readable at once on purpose:
//
//   - ID is the catalog's stable RequirementID, so the UI can key remediation
//     copy on it (and a test can assert WHICH check failed rather than
//     string-matching a sentence);
//   - Fact names the agent datum that was read, so a support conversation can
//     start from "what did the machine actually report" instead of from a
//     guess;
//   - Expected is the catalog's own requirement sentence — the same text the
//     connect dialog showed BEFORE the install, so the two cannot contradict
//     each other;
//   - Detected is what the host reported;
//   - Reason is the whole thing as one sentence, which the UI renders VERBATIM.
//     Rendering it verbatim is what keeps the explanation in one place: a
//     dashboard that re-assembled its own sentence from the parts would drift
//     from the API's, and the two would disagree in exactly the situation where
//     the operator is already confused.
type FailedRequirement struct {
	ID       RequirementID `json:"id"`
	Fact     string        `json:"fact"`
	Expected string        `json:"expected"`
	Detected string        `json:"detected"`
	Reason   string        `json:"reason"`
}

// CheckServerCompatibility evaluates a server type's catalog requirements
// against the facts the agent reported, returning one entry per requirement the
// host violates — empty when the host is compatible OR when nothing could be
// evaluated (see rule 1 above).
//
// An unknown server type returns nil rather than failing: a type outside the
// catalog is refused at enrollment by IsServerType long before this runs, and
// inventing an incompatibility for it here would mark legacy rows — a type
// removed from the catalog after hosts were enrolled as it — incompatible on
// their next heartbeat, with a reason no requirement backs.
func CheckServerCompatibility(serverType string, f HostFacts) []FailedRequirement {
	spec, ok := catalogByType[serverType]
	if !ok {
		return nil
	}
	req := spec.Requires
	// Requirement.Text/ID/Fact come from List() so the failure quotes the same
	// sentence the connect dialog promised; texts are indexed by id rather than
	// re-derived, which is what keeps this walk free of per-type knowledge.
	expected := map[RequirementID]Requirement{}
	for _, r := range req.List() {
		expected[r.ID] = r
	}
	var out []FailedRequirement

	fail := func(id RequirementID, detected, because string) {
		r := expected[id]
		out = append(out, FailedRequirement{
			ID: id, Fact: r.Fact, Expected: r.Text, Detected: clipFact(detected),
			Reason: fmt.Sprintf("You connected this as a %s server, but %s.", spec.Label, clipFact(because)),
		})
	}

	// Distro. The agent reports "" when it could not read any os-release, which
	// is unknown; a rolling release with no VERSION_ID reports its bare id,
	// which is a real answer and can be refused with a real reason.
	if len(req.Distros) > 0 && f.Distro != "" && !containsString(req.Distros, f.Distro) {
		detected := f.Distro
		if f.DistroName != "" {
			// PRETTY_NAME is what the operator will recognise from the machine
			// they just logged into; the id is what our vocabulary uses.
			detected = f.DistroName
		}
		fail(ReqDistro, detected, fmt.Sprintf("it runs %s — that type needs %s",
			detected, joinOr(distroLabelsFor(req.Distros))))
	}

	if len(req.Arches) > 0 && f.Arch != "" && !containsString(req.Arches, f.Arch) {
		fail(ReqArch, f.Arch, fmt.Sprintf("its CPU architecture is %s — that type needs %s",
			f.Arch, joinOr(req.Arches)))
	}

	// Disk. Zero is "the probe could not stat a real filesystem" (the agent
	// skips pseudo-filesystems rather than reporting their zero total), so it
	// is unknown — a host is never refused for a disk nobody measured.
	if req.MinDiskBytes > 0 && f.DiskTotalBytes > 0 && f.DiskTotalBytes < uint64(req.MinDiskBytes) {
		fail(ReqDisk, humanBytes(f.DiskTotalBytes),
			fmt.Sprintf("it has %s of disk — that type needs at least %s",
				humanBytes(f.DiskTotalBytes), formatDiskBytes(req.MinDiskBytes)))
	}

	// GPU. A nil inventory means the agent never looked (it predates the fact,
	// or its probe timed out and the merge kept nothing). An inventory that is
	// PRESENT and empty is the host answering "I looked, there is nothing here"
	// — the whole reason the agent always sends this key — and that is a real
	// reading the gate must act on.
	if req.GPU != nil && f.GPU != nil {
		vendor := strings.ToUpper(req.GPU.Vendor)
		switch {
		case f.GPU.Count == 0 || !strings.EqualFold(f.GPU.Vendor, req.GPU.Vendor):
			detected := "none"
			if f.GPU.Count > 0 {
				detected = fmt.Sprintf("%d × %s", f.GPU.Count, f.GPU.Vendor)
			}
			fail(ReqGPU, detected, fmt.Sprintf("no %s GPU was detected", vendor))
		case req.GPU.Driver && f.GPU.DriverVersion == "":
			// The card enumerates but nothing can drive it. Caught separately
			// because it fails LATER and more expensively than a missing card:
			// the host enrolls, bills at GPU weight, and dies at the first
			// container start.
			fail(ReqGPU, fmt.Sprintf("%d × %s, no driver", f.GPU.Count, f.GPU.Vendor),
				fmt.Sprintf("its %s GPU has no usable driver", vendor))
		}
	}
	return out
}

// compatibilityStatus maps a verdict onto the status the server should hold.
//
// agentCheckedIn is whether the agent has ever been heard from: true for a
// heartbeat by definition, and true on the type-change path when the row
// already has a last_seen_at. It is false at registration, which happens
// seconds before the first heartbeat and has always left liveness to it — so a
// host that clears the gate at register goes to provisioning rather than
// straight to running. One function decides the status, and it never has to
// claim liveness it does not have in order to express "no longer
// incompatible".
func compatibilityStatus(prev string, fails []FailedRequirement, agentCheckedIn bool) string {
	// A decommissioning server outranks the gate, and this guard is load-bearing
	// rather than tidy. The teardown takes minutes, the agent keeps heartbeating
	// throughout it, and one of the two documented exits from `incompatible` IS
	// disconnecting — so the very host most likely to be decommissioned is one
	// whose facts fail its type. Letting the gate write `incompatible` back over
	// `decommissioning` would take the row out of the sweeper's timeout scan
	// (which keys on the status) and off the dashboard's in-flight list, and the
	// operator's disconnect would silently never complete.
	if prev == ServerStatusDecommissioning {
		return prev
	}
	if len(fails) > 0 {
		return ServerStatusIncompatible
	}
	switch {
	case agentCheckedIn && (prev == ServerStatusProvisioning || prev == ServerStatusUnreachable ||
		prev == ServerStatusIncompatible):
		// Recovery is the same transition as first contact. A host that was
		// refused as a GPU server and then had its driver installed must come
		// back on its own next heartbeat — an operator who fixed the machine
		// should not also have to re-enroll it.
		return ServerStatusRunning
	case !agentCheckedIn && prev == ServerStatusIncompatible:
		return ServerStatusProvisioning
	}
	return prev
}

// writeCompatibilityTx persists the verdict. The reasons column is NOT NULL, so
// a compatible host writes an empty array rather than a null: every reader gets
// a list and none of them has to decide what null means.
//
// It writes unconditionally rather than only on a change. The alternative —
// skipping when the status is unchanged — would freeze the DETECTED values
// inside the reasons at whatever they were the first time the host was refused,
// so an operator who grew a disk from 60 GB to 90 GB against a 100 GB floor
// would keep reading "it has 60 GB of disk" while working on it. This runs
// inside the transaction that already updated the row, on the same page.
func writeCompatibilityTx(ctx context.Context, tx pgx.Tx, serverID, status string, fails []FailedRequirement) error {
	reasons := []byte("[]")
	if len(fails) > 0 {
		encoded, err := json.Marshal(fails)
		if err != nil {
			return fmt.Errorf("encode incompatibility: %w", err)
		}
		reasons = encoded
	}
	if _, err := tx.Exec(ctx,
		`UPDATE servers SET status = $2, incompatible_reasons = $3::jsonb WHERE id = $1`,
		serverID, status, reasons); err != nil {
		return fmt.Errorf("record compatibility: %w", err)
	}
	return nil
}

// IncompatibilitySummary is the one-line form for an audit entry: the first
// failure's sentence, plus a count when more than one requirement failed.
// Empty for a compatible host.
func IncompatibilitySummary(fails []FailedRequirement) string {
	if len(fails) == 0 {
		return ""
	}
	if len(fails) == 1 {
		return fails[0].Reason
	}
	return fmt.Sprintf("%s (+%d more)", fails[0].Reason, len(fails)-1)
}

func containsString(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}

// humanBytes renders a REPORTED size the way the operator's provider quotes it
// (decimal GB/TB). Separate from formatDiskBytes, which renders a catalog floor
// and can assume a round number: a real disk is 1968526655488 bytes, and
// "1.968526655488 TB" in a rejection message reads like a bug.
func humanBytes(b uint64) string {
	if b >= 1000*gb {
		return fmt.Sprintf("%.1f TB", float64(b)/float64(1000*gb))
	}
	return fmt.Sprintf("%d GB", b/gb)
}

// maxFactText bounds the agent-supplied fragments that reach a reason.
//
// The reason is rendered VERBATIM in the dashboard, and its inputs — a distro
// name, a GPU model — are strings the host chose. The facts payload is capped
// at 16 KiB, so a 4 KB distroName produced a 4 KB sentence in the server row
// and on screen. Not dangerous (React escapes it, and nothing here is a query),
// but a refusal that scrolls is a refusal nobody reads.
const maxFactText = 120

// clipFact bounds one agent-supplied fragment, keeping the head — the part that
// identifies what was found — rather than truncating to nothing.
func clipFact(v string) string {
	if len(v) <= maxFactText {
		return v
	}
	return strings.TrimSpace(v[:maxFactText]) + "…"
}

// DefaultStaleAfter mirrors the sweeper's StaleAfter (cmd/sigmahub-cp wires 90s).
// It is duplicated rather than plumbed because the type-change path is a single
// store call with no sweeper in reach; a value that drifted low would only ever
// hand back `unreachable` for a host that is in fact alive, which its next
// heartbeat corrects within seconds.
const DefaultStaleAfter = 90 * time.Second

// statusAfterTypeChange is the status a server takes when its TYPE is re-filed.
//
// Separate from compatibilityStatus because the two answer different questions.
// A heartbeat is evidence the host is alive right now; a type change is
// evidence of nothing at all — the operator edited a row, the machine was never
// consulted. Passing `last_seen_at IS NOT NULL` as "the agent checked in"
// conflated "has ever spoken to us" with "is speaking to us now", so re-filing
// the type of a host the sweeper had already given up on wrote `running` over
// `unreachable`: the server became billable (both ConnectedServerUnits and
// SweepServerHours key on 'running') without a single heartbeat, and the
// sweeper flipped it back on its next tick with a second spurious unreachable
// alert for a machine that never came back.
//
// So: a type change may set or clear `incompatible`, and may never invent
// liveness. Clearing it restores the liveness the row actually has, which is
// what last_seen_at says — never seen, seen too long ago, or seen recently.
func statusAfterTypeChange(prev string, fails []FailedRequirement, lastSeenAt *time.Time, staleAfter time.Duration) string {
	// Same terminal rule as compatibilityStatus: a machine on its way out is not
	// re-graded. (The type endpoint refuses outright while a decommission is in
	// flight; this is the belt to that braces.)
	if prev == ServerStatusDecommissioning {
		return prev
	}
	if len(fails) > 0 {
		return ServerStatusIncompatible
	}
	if prev != ServerStatusIncompatible {
		// Nothing to clear, so nothing to decide: a re-file does not promote a
		// provisioning host, nor revive an unreachable one.
		return prev
	}
	switch {
	case lastSeenAt == nil:
		return ServerStatusProvisioning
	case time.Since(*lastSeenAt) > staleAfter:
		// The sweeper would have called this one unreachable had it not been
		// parked in 'incompatible' — it only ever flips rows that read
		// 'running'. Handing it back as 'running' would make the fix look like
		// a recovery the host never performed.
		return ServerStatusUnreachable
	default:
		return ServerStatusRunning
	}
}
