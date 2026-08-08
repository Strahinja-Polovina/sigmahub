package store

import (
	"encoding/json"
	"testing"
)

// The exact payload a current agent produces, decoded into the read model the
// registration gate and the VRAM fit estimate will use. Pasted rather than
// constructed so a rename on either side of the module boundary shows up here.
const agentFactsJSON = `{
  "hostname": "gpu-1", "os": "linux", "arch": "amd64", "kernel": "6.8.0-45-generic",
  "numCpu": 32, "memTotalMb": 128744, "goVersion": "go1.26", "pid": 812,
  "dockerAvailable": true, "dockerVersion": "27.3.1",
  "distro": "ubuntu-24.04", "distroName": "Ubuntu 24.04.1 LTS",
  "diskTotalBytes": 1968526655488, "diskFreeBytes": 1801439850948, "diskPath": "/var/lib/sigmad",
  "gpu": {
    "vendor": "nvidia", "model": "NVIDIA L40S", "count": 2,
    "vramBytesPerGpu": 48301604864, "vramBytesTotal": 96603209728,
    "driverVersion": "550.54.15",
    "cards": [
      {"index": 0, "model": "NVIDIA L40S", "vramBytes": 48301604864},
      {"index": 1, "model": "NVIDIA L40S", "vramBytes": 48301604864}
    ]
  }
}`

func TestParseHostFacts(t *testing.T) {
	f := ParseHostFacts(json.RawMessage(agentFactsJSON))
	if f.Arch != "amd64" || f.Distro != "ubuntu-24.04" || f.DistroName != "Ubuntu 24.04.1 LTS" {
		t.Fatalf("arch/distro = %q/%q/%q", f.Arch, f.Distro, f.DistroName)
	}
	// The reported distro must be in the catalog's own vocabulary — the whole
	// reason the agent normalizes it rather than shipping ID/VERSION_ID raw.
	if !DistroSupported(f.Distro) {
		t.Fatalf("agent-reported distro %q is not a catalog distro; the two vocabularies have diverged", f.Distro)
	}
	if f.DiskTotalBytes != 1968526655488 || f.DiskPath != "/var/lib/sigmad" {
		t.Fatalf("disk = %d at %q", f.DiskTotalBytes, f.DiskPath)
	}
	if f.GPU == nil {
		t.Fatal("gpu inventory did not decode")
	}
	if f.GPU.Vendor != "nvidia" || f.GPU.Count != 2 || f.GPU.DriverVersion != "550.54.15" {
		t.Fatalf("gpu = %+v", *f.GPU)
	}
	// SIGMA-214 does arithmetic on this: a 26 GB model does not fit a 16 GB
	// card, and it can only know that from a per-card BYTE count.
	if f.GPU.VRAMBytesPerGPU != 48301604864 || f.GPU.VRAMBytesTotal != 96603209728 {
		t.Fatalf("vram per/total = %d/%d", f.GPU.VRAMBytesPerGPU, f.GPU.VRAMBytesTotal)
	}
	if len(f.GPU.Cards) != 2 || f.GPU.Cards[1].Index != 1 || f.GPU.Cards[1].VRAMBytes != 48301604864 {
		t.Fatalf("cards = %+v", f.GPU.Cards)
	}
	// The catalog's GPU requirement is stated against this vendor token.
	spec, ok := ServerTypeSpecFor("gpu")
	if !ok || spec.Requires.GPU == nil {
		t.Fatal("gpu server type lost its GPU requirement")
	}
	if spec.Requires.GPU.Vendor != f.GPU.Vendor {
		t.Fatalf("catalog wants vendor %q, agent reports %q", spec.Requires.GPU.Vendor, f.GPU.Vendor)
	}
}

// Everything an old agent sends decodes to "unknown", not to "empty" — the
// distinction the merge depends on.
func TestParseHostFactsOldAgent(t *testing.T) {
	for _, in := range []string{
		`{"hostname":"web-1","os":"linux","arch":"amd64","numCpu":4,"memTotalMb":7900}`,
		`{}`,
		``,
		`not json at all`,
	} {
		f := ParseHostFacts(json.RawMessage(in))
		if f.Distro != "" || f.DiskTotalBytes != 0 || f.GPU != nil {
			t.Fatalf("ParseHostFacts(%q) = %+v, want every SIGMA-201 field unset", in, f)
		}
	}
}

// A host with no accelerator still reports an inventory, so "none" is
// distinguishable from "never asked".
func TestParseHostFactsNoGPU(t *testing.T) {
	f := ParseHostFacts(json.RawMessage(`{"arch":"arm64","gpu":{"vendor":"","count":0}}`))
	if f.GPU == nil {
		t.Fatal("an explicit empty inventory decoded as nil; 'no GPU' and 'unknown' must not collapse")
	}
	if f.GPU.Count != 0 || f.GPU.Vendor != "" {
		t.Fatalf("gpu = %+v", *f.GPU)
	}
}
