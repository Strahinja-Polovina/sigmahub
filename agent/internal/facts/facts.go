// Package facts collects host system facts for registration and heartbeats.
package facts

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
)

type Facts struct {
	Hostname   string `json:"hostname"`
	OS         string `json:"os"`
	Arch       string `json:"arch"`
	Kernel     string `json:"kernel,omitempty"`
	NumCPU     int    `json:"numCpu"`
	MemTotalMB uint64 `json:"memTotalMb,omitempty"`
	GoVer      string `json:"goVersion"`
	PID        int    `json:"pid"`
	// DockerAvailable / DockerVersion report the container runtime the agent can
	// drive (P1-3). Populated by the caller from a short-timeout probe, left
	// zero when Docker is absent so the CP can surface capacity accurately.
	// DockerVersion is sent even when empty: facts MERGE on the control plane,
	// so a key that is merely omitted keeps its previous value, and a host that
	// had Docker removed went on showing the version of a daemon that is no
	// longer installed.
	DockerAvailable bool   `json:"dockerAvailable"`
	DockerVersion   string `json:"dockerVersion"`

	// SIGMA-201. Distro, disk and GPU are the three things the product has to
	// know about a host and could not ask it: the distro was a DROPDOWN the
	// operator guessed at before ever logging into the box, disk was only ever
	// a percentage (useless for "does 500 GB of object storage fit here"), and
	// nothing knew whether a machine had an accelerator at all.
	//
	// Every field here is omitempty on purpose, and the control plane treats an
	// absent key as "unchanged/unknown" rather than "empty" — a probe that
	// could not answer (no /etc/os-release, an unreadable mount) must not wipe
	// the answer a previous heartbeat already established. The one deliberate
	// exception is GPU: see its comment.

	// Distro is the normalized "<id>-<version>" id in the SAME vocabulary as
	// the control plane's catalog (store/server_catalog.go: ubuntu-24.04,
	// debian-12), so a reported value can be fed straight into DistroSupported
	// instead of being re-mapped by every consumer. Empty when os-release is
	// missing or unparsable.
	Distro string `json:"distro,omitempty"`
	// DistroName is os-release PRETTY_NAME, kept for display only. Distro is
	// what anything machine-checkable reads: PRETTY_NAME is marketing copy that
	// changes between point releases ("Ubuntu 24.04.1 LTS" → "…24.04.2 LTS").
	DistroName string `json:"distroName,omitempty"`

	// DiskTotalBytes/DiskFreeBytes are BYTES for the filesystem holding the
	// agent data root — not a percentage. The catalog states disk floors in
	// bytes (databaseMinDisk, storageMinDisk) and a percentage cannot be
	// compared against a floor. DiskPath names the path actually measured so a
	// number is never attributed to the wrong mount.
	//
	// DiskFreeBytes is a POINTER because zero free is the reading that matters
	// most and is genuinely reachable — gopsutil reports the unprivileged-
	// available figure, which hits 0 on ext4 while the root reserve still has
	// room. With a plain uint64 the control plane could not tell "could not
	// stat the mount" from "the disk is full", so it treated both as unknown
	// and a full disk kept advertising its last healthy figure forever.
	DiskTotalBytes uint64  `json:"diskTotalBytes,omitempty"`
	DiskFreeBytes  *uint64 `json:"diskFreeBytes,omitempty"`
	DiskPath       string  `json:"diskPath,omitempty"`

	// GPU is the accelerator inventory. Unlike the fields above it is sent even
	// when the host has none (count 0, vendor ""): "I looked and there is no
	// GPU" and "I never looked" are different answers, and a host that LOSES a
	// card (pulled, or a driver that stopped loading) has to be able to say so.
	// Absent means an agent too old to have looked.
	GPU *GPUInventory `json:"gpu,omitempty"`
}

// Collector gathers host facts. The three host-specific inputs are fields
// rather than direct calls because the interesting cases cannot be reproduced
// on a build machine: CI has no /etc/os-release worth asserting on, no
// nvidia-smi, and no second disk. Tests substitute them; New wires the real OS.
type Collector struct {
	dataRoot string
	// osReleasePaths is tried in order. Two paths because os-release is
	// specified as /etc/os-release with /usr/lib/os-release as the vendor
	// fallback, and minimal container images ship only the latter.
	osReleasePaths []string
	diskUsage      func(ctx context.Context, path string) (total, free uint64, err error)
	// lookPath is kept separate from run so "nvidia-smi is not installed"
	// (the overwhelmingly common case, and not an error) stays distinguishable
	// from "nvidia-smi ran and failed".
	lookPath func(file string) (string, error)
	run      func(ctx context.Context, name string, args ...string) ([]byte, error)
	// probeTimeout is a field rather than the bare const so a test can exercise
	// the timeout path without spending the real bound doing it.
	probeTimeout time.Duration
}

// New builds a collector bound to the real host. dataRoot is the agent's data
// directory — the disk figure describes the filesystem the agent will actually
// fill with images, volumes and backups, which on a real server is frequently
// not the same device as /.
func New(dataRoot string) *Collector {
	return &Collector{
		dataRoot:       dataRoot,
		osReleasePaths: []string{"/etc/os-release", "/usr/lib/os-release"},
		diskUsage: func(ctx context.Context, path string) (uint64, uint64, error) {
			u, err := disk.UsageWithContext(ctx, path)
			if err != nil {
				return 0, 0, err
			}
			return u.Total, u.Free, nil
		},
		probeTimeout: probeTimeout,
		lookPath:     exec.LookPath,
		run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).Output()
		},
	}
}

// Collect gathers host facts; fields that can't be read on a platform stay
// zero rather than failing registration or a heartbeat. Nothing in here may
// return an error: this runs on the heartbeat path, and a host that has grown
// something unreadable still has to check in.
func (c *Collector) Collect(ctx context.Context) Facts {
	hostname, _ := os.Hostname()
	f := Facts{
		Hostname: hostname,
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
		NumCPU:   runtime.NumCPU(),
		GoVer:    runtime.Version(),
		PID:      os.Getpid(),
	}
	if kernel, err := host.KernelVersion(); err == nil {
		f.Kernel = kernel
	}
	if vm, err := mem.VirtualMemory(); err == nil {
		f.MemTotalMB = vm.Total / (1 << 20)
	}
	f.Distro, f.DistroName = c.collectDistro()
	f.DiskPath, f.DiskTotalBytes, f.DiskFreeBytes = c.collectDisk(ctx)
	// An inconclusive GPU probe reports NOTHING, so the merge keeps the last
	// known inventory; a conclusive one always reports, including "none".
	if gpu, ok := c.collectGPU(ctx); ok {
		f.GPU = &gpu
	}
	return f
}

// collectDistro reads the first os-release that exists and parses it. A host
// with neither file (or with an unreadable one) reports empty rather than
// guessing from GOOS — a wrong distro id would be silently accepted by the
// registration gate and then break the package manager calls that follow.
func (c *Collector) collectDistro() (id, pretty string) {
	for _, path := range c.osReleasePaths {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		return ParseOSRelease(string(b))
	}
	return "", ""
}

// collectDisk measures the data root, falling back to the filesystem root when
// the data root cannot be stat'd — an agent whose data dir has not been created
// yet (or was removed underneath it) still knows how big its disk is, and
// reporting nothing would leave the catalog's disk floors unevaluable. The path
// measured is returned so the figure is never silently attributed to the wrong
// mount.
func (c *Collector) collectDisk(ctx context.Context) (path string, total uint64, free *uint64) {
	for _, p := range []string{c.dataRoot, "/"} {
		if p == "" {
			continue
		}
		gotTotal, gotFree, err := c.diskUsage(ctx, p)
		// A zero total is a pseudo-filesystem answering, not a disk: reporting
		// it would read as "this host has no disk" and fail every disk floor.
		// A zero FREE is the opposite — a real, urgent reading — so it is
		// returned as a set pointer rather than skipped.
		if err != nil || gotTotal == 0 {
			continue
		}
		return p, gotTotal, &gotFree
	}
	return "", 0, nil
}

// probeTimeout bounds an external probe. nvidia-smi on a host with a wedged
// driver blocks in the kernel indefinitely; without a bound that stalls the
// heartbeat loop and the control plane marks a perfectly healthy server
// unreachable because its GPU is sulking.
const probeTimeout = 5 * time.Second
