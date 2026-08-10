// Package cpmetrics is the control plane's report on its own health
// (SIGMA-248).
//
// The CP starts six background goroutines — the fleet resync, the deploy
// drain, the backup scheduler, the alert dispatcher, the sweeper and the
// 10-minute usage/billing sweep — and every one of them has the same shape: do
// the work, log an error if it failed, keep ticking. From outside the process
// there was no way to tell a loop that is working from a loop that has been
// erroring on every tick for a day. `CreateDueBackupRuns` starting to fail
// every minute produces no backup runs for any tenant, and no `backup_failed`
// alert either, because that alert needs a run to exist before it can fail.
// The first symptom is somebody noticing, days later, that a resource's last
// backup is old.
//
// So each loop reports the wall-clock time of its last SUCCESSFUL pass, and
// that timestamp is exposed on /metrics for something outside the process to
// alert on staleness. The counters next to it (iterations, errors) say whether
// a loop is spinning or wedged; the last-success gauge is the one that matters,
// because it is the only value that keeps getting worse while nothing is
// happening.
//
// Every canonical loop is registered at construction and therefore always
// present in the output, at 0 until it first succeeds. That is deliberate: a
// loop that died before its first pass, or was never started because a wiring
// mistake dropped its `go` statement, must show up as "last success: never"
// rather than as an absent series. An absent series alerts on nothing.
//
// The exposition is hand-rolled Prometheus text (version 0.0.4). The format is
// a dozen lines of printf and the alternative is a dependency tree an air-gapped
// control plane has to carry for four gauges and two counters.
package cpmetrics

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"
)

// The canonical background loops. Names are stable identifiers — they end up in
// alerting rules, so renaming one silently disables whatever watches it.
const (
	LoopReconcilerResync = "reconciler_resync"
	LoopDeployDrain      = "deploy_drain"
	LoopBackupScheduler  = "backup_scheduler"
	LoopAlertDispatcher  = "alert_dispatcher"
	LoopSweeper          = "sweeper"
	LoopUsageSweep       = "usage_sweep"
)

// Loops is every loop a control plane is expected to be running. Registered up
// front so each one is reported even before (or instead of) its first pass.
var Loops = []string{
	LoopReconcilerResync,
	LoopDeployDrain,
	LoopBackupScheduler,
	LoopAlertDispatcher,
	LoopSweeper,
	LoopUsageSweep,
}

// PoolStats is the pgxpool snapshot worth exporting: a control plane whose
// connections are all checked out is about to stall every loop at once, and
// that is visible here minutes before it is visible anywhere else.
type PoolStats struct {
	Acquired int32
	Idle     int32
	Total    int32
	Max      int32
}

type loopState struct {
	// lastSuccess is zero until the loop completes a pass without error.
	lastSuccess time.Time
	iterations  uint64
	errors      uint64
}

// Registry holds the control plane's self-reported health. Safe for concurrent
// use; a nil *Registry is usable and every method on it is a no-op, so callers
// that were not given one need no branch.
type Registry struct {
	mu        sync.Mutex
	loops     map[string]*loopState
	pool      func() PoolStats
	startedAt time.Time
	// DSD render latency, as a Prometheus summary's two required series
	// (_sum/_count). The render sits on the agent's poll path, so a slow render
	// is a fleet-wide delivery delay rather than a local inconvenience.
	renderCount   uint64
	renderSeconds float64
	// Fleet-resync pass duration (SIGMA-320). The last-success clock above says
	// whether the resync is alive; this says whether it still meets the 60s
	// drift-repair interval everything downstream assumes. A pass that grows
	// past the tick is not an error and never will be — it just makes every
	// convergence slower, silently, so the pass has to be measured rather than
	// inferred. The last value is exported alongside the summary because "the
	// most recent pass took 240 seconds" is the sentence an operator needs.
	resyncPassCount   uint64
	resyncPassSeconds float64
	resyncPassLast    float64
}

func New() *Registry {
	r := &Registry{loops: map[string]*loopState{}, startedAt: time.Now()}
	for _, name := range Loops {
		r.loops[name] = &loopState{}
	}
	return r
}

// Loop returns the handle a background loop reports through. Unknown names are
// registered on the spot rather than dropped — losing a heartbeat because a
// caller used a name this package had not heard of would be the exact failure
// this package exists to prevent.
func (r *Registry) Loop(name string) *Loop {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.loops[name]; !ok {
		r.loops[name] = &loopState{}
	}
	return &Loop{reg: r, name: name}
}

// Loop is one background loop's reporting handle.
type Loop struct {
	reg  *Registry
	name string
}

// Pass records one iteration that did all of its work. This is what moves the
// last-success clock, so it must be called only when the pass actually
// succeeded — a loop that reports Pass() on its way past a logged error is
// exactly as undetectable as one that reports nothing.
func (l *Loop) Pass() { l.record(true) }

// Fail records one iteration that did not. It advances the iteration and error
// counters and deliberately leaves the last-success clock alone.
func (l *Loop) Fail() { l.record(false) }

// Report is the callback shape the background loops take: one call per pass,
// carrying whatever that pass ended with. Method value on a *Loop, so wiring a
// loop up is `Heartbeat: reg.Loop(name).Report` and a loop with no registry
// keeps working because a nil *Loop's methods do nothing.
func (l *Loop) Report(err error) {
	if err != nil {
		l.Fail()
		return
	}
	l.Pass()
}

func (l *Loop) record(ok bool) {
	if l == nil || l.reg == nil {
		return
	}
	l.reg.mu.Lock()
	defer l.reg.mu.Unlock()
	st := l.reg.loops[l.name]
	if st == nil {
		st = &loopState{}
		l.reg.loops[l.name] = st
	}
	st.iterations++
	if ok {
		st.lastSuccess = time.Now()
	} else {
		st.errors++
	}
}

// SetPoolSource installs the database-pool sampler, read at scrape time so the
// numbers are current rather than as-of the last tick of something else.
func (r *Registry) SetPoolSource(fn func() PoolStats) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pool = fn
}

// ObserveDSDRender records how long one server's DSD render took.
func (r *Registry) ObserveDSDRender(d time.Duration) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.renderCount++
	r.renderSeconds += d.Seconds()
}

// ObserveResyncPass records how long one whole fleet-resync pass took
// (SIGMA-320).
func (r *Registry) ObserveResyncPass(d time.Duration) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resyncPassCount++
	r.resyncPassSeconds += d.Seconds()
	r.resyncPassLast = d.Seconds()
}

// WritePrometheus renders the registry in the Prometheus text exposition
// format.
func (r *Registry) WritePrometheus(w io.Writer) {
	if r == nil {
		return
	}
	r.mu.Lock()
	names := make([]string, 0, len(r.loops))
	for name := range r.loops {
		names = append(names, name)
	}
	sort.Strings(names)
	type row struct {
		name        string
		lastSuccess float64
		iterations  uint64
		errors      uint64
	}
	rows := make([]row, 0, len(names))
	for _, name := range names {
		st := r.loops[name]
		var last float64
		if !st.lastSuccess.IsZero() {
			last = float64(st.lastSuccess.UnixNano()) / 1e9
		}
		rows = append(rows, row{name, last, st.iterations, st.errors})
	}
	poolFn := r.pool
	startedAt := r.startedAt
	renderCount, renderSeconds := r.renderCount, r.renderSeconds
	passCount, passSeconds, passLast := r.resyncPassCount, r.resyncPassSeconds, r.resyncPassLast
	r.mu.Unlock()

	fmt.Fprint(w, "# HELP sigmahub_cp_loop_last_success_seconds Unix time of the last fully successful pass of a control-plane background loop; 0 means it has never completed one.\n")
	fmt.Fprint(w, "# TYPE sigmahub_cp_loop_last_success_seconds gauge\n")
	for _, rw := range rows {
		fmt.Fprintf(w, "sigmahub_cp_loop_last_success_seconds{loop=%q} %.3f\n", rw.name, rw.lastSuccess)
	}
	fmt.Fprint(w, "# HELP sigmahub_cp_loop_iterations_total Passes attempted by a control-plane background loop.\n")
	fmt.Fprint(w, "# TYPE sigmahub_cp_loop_iterations_total counter\n")
	for _, rw := range rows {
		fmt.Fprintf(w, "sigmahub_cp_loop_iterations_total{loop=%q} %d\n", rw.name, rw.iterations)
	}
	fmt.Fprint(w, "# HELP sigmahub_cp_loop_errors_total Passes of a control-plane background loop that failed.\n")
	fmt.Fprint(w, "# TYPE sigmahub_cp_loop_errors_total counter\n")
	for _, rw := range rows {
		fmt.Fprintf(w, "sigmahub_cp_loop_errors_total{loop=%q} %d\n", rw.name, rw.errors)
	}

	fmt.Fprint(w, "# HELP sigmahub_cp_start_time_seconds Unix time this control-plane process started.\n")
	fmt.Fprint(w, "# TYPE sigmahub_cp_start_time_seconds gauge\n")
	fmt.Fprintf(w, "sigmahub_cp_start_time_seconds %.3f\n", float64(startedAt.UnixNano())/1e9)

	fmt.Fprint(w, "# HELP sigmahub_cp_dsd_render_seconds Time spent rendering server desired-state documents.\n")
	fmt.Fprint(w, "# TYPE sigmahub_cp_dsd_render_seconds summary\n")
	fmt.Fprintf(w, "sigmahub_cp_dsd_render_seconds_sum %.6f\n", renderSeconds)
	fmt.Fprintf(w, "sigmahub_cp_dsd_render_seconds_count %d\n", renderCount)

	fmt.Fprint(w, "# HELP sigmahub_cp_resync_pass_seconds Time taken by a whole fleet-resync pass.\n")
	fmt.Fprint(w, "# TYPE sigmahub_cp_resync_pass_seconds summary\n")
	fmt.Fprintf(w, "sigmahub_cp_resync_pass_seconds_sum %.6f\n", passSeconds)
	fmt.Fprintf(w, "sigmahub_cp_resync_pass_seconds_count %d\n", passCount)
	fmt.Fprint(w, "# HELP sigmahub_cp_resync_pass_last_seconds Duration of the most recent fleet-resync pass; compare against the resync interval (60s), which is the fleet's drift-repair SLO.\n")
	fmt.Fprint(w, "# TYPE sigmahub_cp_resync_pass_last_seconds gauge\n")
	fmt.Fprintf(w, "sigmahub_cp_resync_pass_last_seconds %.6f\n", passLast)

	if poolFn != nil {
		p := poolFn()
		fmt.Fprint(w, "# HELP sigmahub_cp_db_pool_connections Database pool connections by state.\n")
		fmt.Fprint(w, "# TYPE sigmahub_cp_db_pool_connections gauge\n")
		fmt.Fprintf(w, "sigmahub_cp_db_pool_connections{state=%q} %d\n", "acquired", p.Acquired)
		fmt.Fprintf(w, "sigmahub_cp_db_pool_connections{state=%q} %d\n", "idle", p.Idle)
		fmt.Fprintf(w, "sigmahub_cp_db_pool_connections{state=%q} %d\n", "total", p.Total)
		fmt.Fprint(w, "# HELP sigmahub_cp_db_pool_max_connections Configured pool ceiling.\n")
		fmt.Fprint(w, "# TYPE sigmahub_cp_db_pool_max_connections gauge\n")
		fmt.Fprintf(w, "sigmahub_cp_db_pool_max_connections %d\n", p.Max)
	}
}

// ServeHTTP makes the registry its own scrape handler.
func (r *Registry) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	r.WritePrometheus(w)
}
