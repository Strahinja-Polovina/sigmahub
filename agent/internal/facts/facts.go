// Package facts collects host system facts for registration and heartbeats.
// Stdlib-only in v0: no cgo, no external deps, cross-platform.
package facts

import (
	"os"
	"runtime"
)

type Facts struct {
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	NumCPU   int    `json:"numCpu"`
	GoVer    string `json:"goVersion"`
	PID      int    `json:"pid"`
}

func Collect() Facts {
	host, _ := os.Hostname()
	return Facts{
		Hostname: host,
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
		NumCPU:   runtime.NumCPU(),
		GoVer:    runtime.Version(),
		PID:      os.Getpid(),
	}
}
