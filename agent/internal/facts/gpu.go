package facts

import (
	"context"
	"strconv"
	"strings"
)

// VendorNVIDIA is the vendor token reported for NVIDIA hardware. It must match
// the control plane catalog's GPURequirement.Vendor byte-for-byte (the two
// modules cannot share Go types, so the wire value is duplicated the same way
// the DSD op kinds are).
const VendorNVIDIA = "nvidia"

// GPUCard is one accelerator exactly as the host enumerates it.
type GPUCard struct {
	Index int    `json:"index"`
	Model string `json:"model"`
	// VRAMBytes is BYTES. nvidia-smi reports MiB and marketing reports "24GB";
	// neither is the number a model has to fit into. An A10G advertised as 24GB
	// enumerates 22731 MiB ≈ 23.8 GB, so a fit check done in advertised GB
	// approves a model that then OOMs on load.
	VRAMBytes uint64 `json:"vramBytes"`
}

// GPUInventory is the host's accelerator inventory.
//
// Shaped for its second consumer as much as for the dashboard: SIGMA-214 sizes
// an LLM (weights × dtype factor + KV-cache headroom) and has to answer "needs
// ~26 GB, this server has 16 GB". That answer needs a NUMBER OF BYTES PER CARD,
// which is why the per-card figure is the primary datum and the totals are
// derived from it — a model that does not fit on one card does not fit just
// because two cards add up, and a rounded "24GB" string cannot be arithmetic'd
// at all.
type GPUInventory struct {
	// Vendor is "" when there is no GPU, which is how a host says "I looked and
	// found nothing" as opposed to saying nothing at all.
	Vendor string `json:"vendor"`
	// Model is set only when every card is the same model. A mixed box has no
	// single honest answer, so readers that care must walk Cards.
	Model string `json:"model,omitempty"`
	Count int    `json:"count"`
	// VRAMBytesPerGPU is the SMALLEST card's VRAM, not the average or the
	// largest. A fit estimate that overstates capacity is worse than one that
	// understates it: the first ends in a failed deploy on a server the operator
	// already paid GPU rates for, the second only in a conservative warning.
	VRAMBytesPerGPU uint64 `json:"vramBytesPerGpu,omitempty"`
	// VRAMBytesTotal is the sum across cards — meaningful only for workloads
	// that shard across them, never for a single-card fit check.
	VRAMBytesTotal uint64 `json:"vramBytesTotal,omitempty"`
	// DriverVersion is empty when no usable driver answered. The catalog's
	// GPURequirement checks this independently of the card count on purpose: a
	// card that enumerates over PCI but has no working kernel driver fails at
	// the first container start, long after the host was enrolled and billed.
	DriverVersion string    `json:"driverVersion,omitempty"`
	Cards         []GPUCard `json:"cards,omitempty"`
}

// nvidiaSMIArgs is the query. --format=csv,noheader,nounits gives one bare row
// per card with memory as a plain MiB integer; the parser tolerates a unit
// suffix anyway, because a host with an older nvidia-smi that ignores nounits
// should degrade to a right answer rather than to zero VRAM.
var nvidiaSMIArgs = []string{
	"--query-gpu=index,name,memory.total,driver_version",
	"--format=csv,noheader,nounits",
}

// collectGPU enumerates accelerators, distinguishing three outcomes because the
// control plane merges facts and therefore treats absent and empty differently:
//
//   - nvidia-smi ABSENT → an empty inventory, reported. That is not an error:
//     every non-GPU server in the fleet is in exactly that state, and a machine
//     whose card was pulled must be able to say so or it advertises the card
//     forever.
//   - nvidia-smi present but the PROBE DID NOT COMPLETE (timeout) → nothing
//     reported, so the merge leaves the last known inventory standing. A
//     five-second deadline on a busy card is not evidence that the card is
//     gone, and reporting an empty inventory here wiped a real 2×L40S box on
//     one slow heartbeat — which SIGMA-203's gate and SIGMA-214's fit check
//     would both then act on.
//   - nvidia-smi present and FAILING (driver not loaded) → an empty inventory,
//     reported. A driver that will not answer cannot run a model either, and
//     enrolling the host as GPU-capable on the strength of the binary existing
//     is the mistake the catalog's Driver requirement exists to prevent.
//
// ok=false is the middle case: "ask again next heartbeat".
func (c *Collector) collectGPU(ctx context.Context) (GPUInventory, bool) {
	if _, err := c.lookPath("nvidia-smi"); err != nil {
		return GPUInventory{}, true
	}
	probeCtx, cancel := context.WithTimeout(ctx, c.probeTimeout)
	defer cancel()
	out, err := c.run(probeCtx, "nvidia-smi", nvidiaSMIArgs...)
	if err != nil {
		// Only a deadline/cancellation is inconclusive. A non-zero exit is the
		// driver answering "no", which is a real reading.
		if probeCtx.Err() != nil {
			return GPUInventory{}, false
		}
		return GPUInventory{}, true
	}
	return parseNvidiaSMI(string(out)), true
}

// parseNvidiaSMI builds the inventory from CSV rows of
// index,name,memory.total,driver_version.
func parseNvidiaSMI(out string) GPUInventory {
	var inv GPUInventory
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) < 4 {
			continue
		}
		// The model name is the only free-text column, so it is what a comma
		// could ever appear in. Anchor on the fixed columns at both ends and
		// treat everything between as the name, rather than trusting a field
		// count that one product name could break.
		idx, err := strconv.Atoi(strings.TrimSpace(fields[0]))
		if err != nil {
			continue
		}
		mib, err := parseMiB(fields[len(fields)-2])
		if err != nil {
			continue
		}
		name := strings.TrimSpace(strings.Join(fields[1:len(fields)-2], ","))
		driver := strings.TrimSpace(fields[len(fields)-1])

		inv.Cards = append(inv.Cards, GPUCard{
			Index:     idx,
			Model:     name,
			VRAMBytes: mib * 1024 * 1024,
		})
		if inv.DriverVersion == "" {
			inv.DriverVersion = driver
		}
	}
	if len(inv.Cards) == 0 {
		// nvidia-smi answered but described no card we could read. Report the
		// same thing as "no tool": pretending to have hardware we cannot
		// describe is what would let a GPU-only workload be scheduled here.
		return GPUInventory{}
	}

	inv.Vendor = VendorNVIDIA
	inv.Count = len(inv.Cards)
	inv.Model = inv.Cards[0].Model
	inv.VRAMBytesPerGPU = inv.Cards[0].VRAMBytes
	for _, card := range inv.Cards {
		inv.VRAMBytesTotal += card.VRAMBytes
		if card.VRAMBytes < inv.VRAMBytesPerGPU {
			inv.VRAMBytesPerGPU = card.VRAMBytes
		}
		if card.Model != inv.Model {
			inv.Model = ""
		}
	}
	return inv
}

// parseMiB reads nvidia-smi's memory column. With nounits it is a bare integer
// of MiB; without it, "22731 MiB". Fractions appear on some driver versions, so
// parse as a float and truncate — 0.5 MiB of VRAM never decided a fit.
func parseMiB(v string) (uint64, error) {
	v = strings.TrimSpace(v)
	if i := strings.IndexFunc(v, func(r rune) bool {
		return !(r >= '0' && r <= '9') && r != '.'
	}); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	mib, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, err
	}
	if mib <= 0 {
		return 0, strconv.ErrRange
	}
	return uint64(mib), nil
}
