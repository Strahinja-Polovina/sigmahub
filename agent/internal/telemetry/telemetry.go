// Package telemetry is sigmad's P1-13 shipper: host + per-container metrics
// every 15s and container stdout/stderr tails, both over the existing
// outbound authenticated agent channel (recorded deviation from "over the
// mesh" — the telemetry sink is not an org-mesh peer). Cardinality control is
// enforced HERE, pre-egress: only the allowlisted labels ever leave the host
// and the active-series cap drops overflow (counted, not silent).
package telemetry

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/agent/internal/client"
	"github.com/Strahinja-Polovina/sigmahub/agent/internal/container"
	"github.com/Strahinja-Polovina/sigmahub/agent/internal/metrics"
)

const (
	metricsInterval = 15 * time.Second
	logsFlush       = 3 * time.Second
	tailerResync    = 10 * time.Second
	// seriesCap bounds active series per push (SIGMA-52's ~20k per server).
	seriesCap = 20000
	// logBufferCap bounds buffered lines between flushes; overflow is dropped
	// and counted (backpressure must never grow agent memory unbounded).
	logBufferCap = 4000
	// disabledBackoff is how long the shipper sleeps after the CP answers
	// "no sink configured", so an unconfigured pipeline costs ~nothing.
	disabledBackoff = 5 * time.Minute
)

// Docker is the runtime slice the shipper needs.
type Docker interface {
	ContainerList(ctx context.Context) ([]container.ContainerState, error)
	ContainerStatsOnce(ctx context.Context, id string) (cpuPct float64, memBytes int64, err error)
	FollowContainerLogs(ctx context.Context, id string, since time.Time, fn func(stream string, ts time.Time, line string)) error
}

// ShipMetrics / ShipLogs post one batch to the CP.
type ShipMetrics func(ctx context.Context, samples []client.TelemetrySample, dropped int) (client.TelemetryAck, error)
type ShipLogs func(ctx context.Context, streams []client.TelemetryLogStream, dropped int) (client.TelemetryAck, error)

type bufferedLine struct {
	resource, service, stream string
	ts                        time.Time
	text                      string
}

// Shipper runs the metric collector and the log tailer manager.
type Shipper struct {
	docker      Docker
	shipMetrics ShipMetrics
	shipLogs    ShipLogs
	log         *slog.Logger

	mu      sync.Mutex
	lines   []bufferedLine
	dropped int
	tailers map[string]bool // container id -> tailer running

	metricsDisabledUntil time.Time
	logsDisabledUntil    time.Time
}

func New(docker Docker, shipMetrics ShipMetrics, shipLogs ShipLogs, log *slog.Logger) *Shipper {
	return &Shipper{
		docker:      docker,
		shipMetrics: shipMetrics,
		shipLogs:    shipLogs,
		log:         log,
		tailers:     map[string]bool{},
	}
}

// Run starts the collector loops and blocks until ctx is done.
func (s *Shipper) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); s.runMetrics(ctx) }()
	go func() { defer wg.Done(); s.runTailerManager(ctx) }()
	go func() { defer wg.Done(); s.runLogFlusher(ctx) }()
	wg.Wait()
}

// managedContainerLabels extracts the allowlisted labels from a managed
// container; ok=false for the Traefik proxy and other unlabeled containers.
func managedContainerLabels(c container.ContainerState) (resource, service string, ok bool) {
	resource = c.Labels[container.LabelResourceID]
	if resource == "" {
		return "", "", false
	}
	return resource, c.Labels[container.LabelService], true
}

func (s *Shipper) runMetrics(ctx context.Context) {
	ticker := time.NewTicker(metricsInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if time.Now().Before(s.metricsDisabledUntil) {
			continue
		}
		now := time.Now().UnixMilli()
		host := metrics.Collect(ctx)
		samples := []client.TelemetrySample{
			{Name: "sigmahub_host_cpu_pct", Value: host.CPUPct, TS: now},
			{Name: "sigmahub_host_mem_pct", Value: host.MemPct, TS: now},
			{Name: "sigmahub_host_disk_pct", Value: host.DiskPct, TS: now},
			{Name: "sigmahub_host_load1", Value: host.Load1, TS: now},
		}
		if list, err := s.docker.ContainerList(ctx); err == nil {
			for _, c := range list {
				if !c.Running {
					continue
				}
				resource, service, ok := managedContainerLabels(c)
				if !ok {
					continue
				}
				cpu, mem, err := s.docker.ContainerStatsOnce(ctx, c.ID)
				if err != nil {
					continue
				}
				labels := map[string]string{"resource": resource}
				if service != "" {
					labels["service"] = service
				}
				samples = append(samples,
					client.TelemetrySample{Name: "sigmahub_container_cpu_pct", Labels: labels, Value: cpu, TS: now},
					client.TelemetrySample{Name: "sigmahub_container_mem_bytes", Labels: labels, Value: float64(mem), TS: now},
				)
			}
		}
		dropped := 0
		if len(samples) > seriesCap {
			dropped = len(samples) - seriesCap
			samples = samples[:seriesCap]
		}
		shipCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		ack, err := s.shipMetrics(shipCtx, samples, dropped)
		cancel()
		if err != nil {
			s.log.Warn("telemetry: metrics ship failed", "err", err)
			continue
		}
		if !ack.Accepted {
			// No sink configured: back off quietly instead of spamming.
			s.metricsDisabledUntil = time.Now().Add(disabledBackoff)
		}
	}
}

// runTailerManager keeps one log tailer per running managed container.
func (s *Shipper) runTailerManager(ctx context.Context) {
	ticker := time.NewTicker(tailerResync)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		list, err := s.docker.ContainerList(ctx)
		if err != nil {
			continue
		}
		for _, c := range list {
			if !c.Running {
				continue
			}
			resource, service, ok := managedContainerLabels(c)
			if !ok {
				continue
			}
			s.mu.Lock()
			running := s.tailers[c.ID]
			if !running {
				s.tailers[c.ID] = true
			}
			s.mu.Unlock()
			if running {
				continue
			}
			go func(id, resource, service string) {
				defer func() {
					s.mu.Lock()
					delete(s.tailers, id)
					s.mu.Unlock()
				}()
				err := s.docker.FollowContainerLogs(ctx, id, time.Now(), func(stream string, ts time.Time, line string) {
					s.mu.Lock()
					if len(s.lines) >= logBufferCap {
						s.dropped++
					} else {
						s.lines = append(s.lines, bufferedLine{resource: resource, service: service, stream: stream, ts: ts, text: line})
					}
					s.mu.Unlock()
				})
				if err != nil && ctx.Err() == nil {
					s.log.Debug("telemetry: log tail ended", "container", id, "err", err)
				}
			}(c.ID, resource, service)
		}
	}
}

func (s *Shipper) runLogFlusher(ctx context.Context) {
	ticker := time.NewTicker(logsFlush)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		s.mu.Lock()
		lines := s.lines
		dropped := s.dropped
		s.lines = nil
		s.dropped = 0
		s.mu.Unlock()
		if time.Now().Before(s.logsDisabledUntil) {
			continue // sink off: drain and drop, cheapest honest behaviour
		}
		if len(lines) == 0 && dropped == 0 {
			continue
		}
		// Group into (resource, service, stream) Loki-shaped streams.
		type key struct{ resource, service, stream string }
		grouped := map[key][]client.TelemetryLogLine{}
		for _, l := range lines {
			k := key{l.resource, l.service, l.stream}
			grouped[k] = append(grouped[k], client.TelemetryLogLine{TS: l.ts.UnixMilli(), Text: l.text})
		}
		streams := make([]client.TelemetryLogStream, 0, len(grouped))
		for k, ls := range grouped {
			streams = append(streams, client.TelemetryLogStream{
				ResourceID: k.resource, Service: k.service, Stream: k.stream, Lines: ls,
			})
		}
		shipCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		ack, err := s.shipLogs(shipCtx, streams, dropped)
		cancel()
		if err != nil {
			s.log.Warn("telemetry: logs ship failed", "err", err)
			continue
		}
		if !ack.Accepted {
			s.logsDisabledUntil = time.Now().Add(disabledBackoff)
		}
	}
}
