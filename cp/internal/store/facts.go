package store

// Host facts: the agent-reported description of a machine (SIGMA-201).
//
// Two rules govern everything in this file, and both exist because the agent
// fleet is NOT versioned in lockstep with the control plane:
//
//  1. An absent key means "unchanged/unknown", never "empty". An agent that
//     predates a fact cannot be allowed to erase it, and neither can a current
//     agent whose probe failed this tick — a server does not stop having 2 TB
//     of disk because one heartbeat could not stat the mount. Facts are
//     therefore MERGED into the stored object rather than replacing it, and any
//     fact reported as its zero value is dropped before the merge so it cannot
//     overwrite a real one.
//
//  2. The single deliberate exception is `gpu`, which a current agent always
//     sends even when the host has none. "I looked and found nothing" has to be
//     expressible, or a machine whose card was pulled — or whose driver stopped
//     loading — would keep advertising hardware it no longer has, and keep
//     being scheduled model workloads it can no longer run.

// normalizeFacts, which enforces rule 1 on the way in, lives next to the
// registration path it guards in registry.go.

import "encoding/json"

// GPUCard is one accelerator as the host enumerated it.
type GPUCard struct {
	Index     int    `json:"index"`
	Model     string `json:"model"`
	VRAMBytes uint64 `json:"vramBytes"`
}

// GPUInventory mirrors the agent's facts.GPUInventory on the wire. The two
// modules cannot share Go types, so the JSON tags here are the contract; they
// are asserted against a real agent payload in the integration test.
//
// VRAM is carried in BYTES per card, not as a rounded "24GB" label, because the
// second consumer is arithmetic: SIGMA-214 sizes a model (weights × dtype
// factor + KV-cache headroom) and has to answer "needs ~26 GB, this server has
// 16 GB". VRAMBytesPerGPU is the smallest card's figure — a fit answer that
// overstates capacity costs a failed deploy on a host already billed at GPU
// rates, one that understates it costs a warning.
type GPUInventory struct {
	Vendor          string    `json:"vendor"`
	Model           string    `json:"model,omitempty"`
	Count           int       `json:"count"`
	VRAMBytesPerGPU uint64    `json:"vramBytesPerGpu,omitempty"`
	VRAMBytesTotal  uint64    `json:"vramBytesTotal,omitempty"`
	DriverVersion   string    `json:"driverVersion,omitempty"`
	Cards           []GPUCard `json:"cards,omitempty"`
}

// HostFacts is the typed read model over the facts JSONB blob.
//
// It covers exactly the keys that ServerRequirements.List() names in its
// Requirement.Fact column — arch, distro, diskTotalBytes, gpu — so the
// registration gate (SIGMA-203) reads a struct instead of every caller writing
// its own `facts->'gpu'->>'vramBytesPerGpu'` with its own idea of what a
// missing value means. That divergence is the failure mode this codebase keeps
// paying for; one decoder, used by both entry points, is the fix.
type HostFacts struct {
	Arch           string        `json:"arch"`
	Distro         string        `json:"distro"`
	DistroName     string        `json:"distroName"`
	DiskTotalBytes uint64        `json:"diskTotalBytes"`
	DiskFreeBytes  uint64        `json:"diskFreeBytes"`
	DiskPath       string        `json:"diskPath"`
	GPU            *GPUInventory `json:"gpu"`
}

// ParseHostFacts decodes the SIGMA-201 subset of a facts blob. Anything
// unparsable — an old agent, a truncated payload, a scalar — yields the zero
// value, whose every field already means "unknown". Never returns an error:
// nothing about a heartbeat should fail because a host described itself badly.
func ParseHostFacts(facts json.RawMessage) HostFacts {
	var f HostFacts
	if err := json.Unmarshal(normalizeFacts(facts), &f); err != nil {
		return HostFacts{}
	}
	return f
}
