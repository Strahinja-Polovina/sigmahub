package cpmetrics

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func render(t *testing.T, r *Registry) string {
	t.Helper()
	var sb strings.Builder
	r.WritePrometheus(&sb)
	return sb.String()
}

// A loop that has never run must still appear, at zero. An absent series alerts
// on nothing, which is the whole failure SIGMA-248 is about.
func TestEveryLoopIsReportedBeforeItEverRuns(t *testing.T) {
	out := render(t, New())
	for _, name := range Loops {
		want := `sigmahub_cp_loop_last_success_seconds{loop="` + name + `"} 0.000`
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

// Fail() counts the iteration and the error and deliberately does NOT move the
// last-success clock — a loop erroring on every tick has to look stale.
func TestFailDoesNotAdvanceLastSuccess(t *testing.T) {
	r := New()
	l := r.Loop(LoopBackupScheduler)
	l.Report(errors.New("enqueue: lock timeout"))
	l.Report(errors.New("enqueue: lock timeout"))

	out := render(t, r)
	if !strings.Contains(out, `sigmahub_cp_loop_last_success_seconds{loop="backup_scheduler"} 0.000`) {
		t.Fatalf("failing passes moved the last-success clock:\n%s", out)
	}
	if !strings.Contains(out, `sigmahub_cp_loop_errors_total{loop="backup_scheduler"} 2`) {
		t.Fatalf("errors not counted:\n%s", out)
	}
	if !strings.Contains(out, `sigmahub_cp_loop_iterations_total{loop="backup_scheduler"} 2`) {
		t.Fatalf("iterations not counted:\n%s", out)
	}

	l.Report(nil)
	out = render(t, r)
	if strings.Contains(out, `sigmahub_cp_loop_last_success_seconds{loop="backup_scheduler"} 0.000`) {
		t.Fatalf("a successful pass did not stamp the clock:\n%s", out)
	}
}

// A loop wired to no registry must be a no-op rather than a panic, so a caller
// that was given no registry needs no nil check.
func TestNilRegistryIsUsable(t *testing.T) {
	var r *Registry
	r.Loop("whatever").Report(errors.New("boom"))
	r.ObserveDSDRender(0)
	r.ObserveResyncPass(0)
	r.SetPoolSource(nil)
	render(t, r)
}

// SIGMA-320: the pass duration is the series that says whether the fleet's 60s
// drift-repair interval is still real, so it has to be exported even before the
// first pass — an absent series alerts on nothing, same as the loop clocks.
func TestResyncPassDurationIsExported(t *testing.T) {
	r := New()
	if out := render(t, r); !strings.Contains(out, "sigmahub_cp_resync_pass_last_seconds 0.000000") {
		t.Fatalf("pass duration missing before the first pass:\n%s", out)
	}
	r.ObserveResyncPass(90 * time.Second)
	out := render(t, r)
	if !strings.Contains(out, "sigmahub_cp_resync_pass_last_seconds 90.000000") {
		t.Fatalf("last pass duration not reported:\n%s", out)
	}
	if !strings.Contains(out, "sigmahub_cp_resync_pass_seconds_count 1") {
		t.Fatalf("pass count not reported:\n%s", out)
	}
}

func TestPoolStatsAreSampledAtScrape(t *testing.T) {
	r := New()
	r.SetPoolSource(func() PoolStats {
		return PoolStats{Acquired: 7, Idle: 3, Total: 10, Max: 20}
	})
	out := render(t, r)
	if !strings.Contains(out, `sigmahub_cp_db_pool_connections{state="acquired"} 7`) {
		t.Fatalf("pool stats missing:\n%s", out)
	}
	if !strings.Contains(out, "sigmahub_cp_db_pool_max_connections 20") {
		t.Fatalf("pool ceiling missing:\n%s", out)
	}
}
