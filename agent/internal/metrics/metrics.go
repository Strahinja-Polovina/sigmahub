// Package metrics samples real host resource usage for heartbeats.
package metrics

import (
	"context"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
)

// Sample is one host resource reading. Percentages are 0–100.
type Sample struct {
	CPUPct  float64 `json:"cpuPct"`
	MemPct  float64 `json:"memPct"`
	DiskPct float64 `json:"diskPct"`
	Load1   float64 `json:"load1"`
}

// Collect reads current host CPU/mem/disk/load. Fields that can't be read on a
// given platform stay zero rather than failing the whole heartbeat.
func Collect(ctx context.Context) Sample {
	var s Sample

	// CPU: short blocking sample of total utilization across all cores.
	if pcts, err := cpu.PercentWithContext(ctx, 200*time.Millisecond, false); err == nil && len(pcts) > 0 {
		s.CPUPct = round2(pcts[0])
	}
	if vm, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		s.MemPct = round2(vm.UsedPercent)
	}
	if du, err := disk.UsageWithContext(ctx, "/"); err == nil {
		s.DiskPct = round2(du.UsedPercent)
	}
	if avg, err := load.AvgWithContext(ctx); err == nil {
		s.Load1 = round2(avg.Load1)
	}
	return s
}

func round2(f float64) float64 {
	return float64(int64(f*100+0.5)) / 100
}
