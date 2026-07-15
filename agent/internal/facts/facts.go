// Package facts collects host system facts for registration and heartbeats.
package facts

import (
	"os"
	"runtime"

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
	DockerAvailable bool   `json:"dockerAvailable"`
	DockerVersion   string `json:"dockerVersion,omitempty"`
}

// Collect gathers host facts; fields that can't be read on a platform stay
// zero rather than failing registration or a heartbeat.
func Collect() Facts {
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
	return f
}
