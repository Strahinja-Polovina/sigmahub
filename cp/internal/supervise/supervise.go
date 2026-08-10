// Package supervise contains the one thing standing between a panic in a
// background loop and total unavailability (SIGMA-250).
//
// net/http recovers panics in HTTP handlers, so a bad request takes down one
// request. Nothing recovered the control plane's background goroutines — the
// fleet resync, the deploy drain, the backup scheduler, the alert dispatcher,
// the sweeper and the usage/billing sweep — so a panic in any of them
// terminated the process for every tenant at once.
//
// The shape of that failure is worse than a crash, because these loops are
// level-triggered and periodic: the supervisor restarts the process, the loop
// reaches the same state a tick later, and it exits again. Every org loses the
// dashboard, deploys, backups, alerting and agent long-polls in a restart loop
// driven by one bad row, and it keeps going until a human identifies and edits
// that row by hand.
//
// Recovering per ITERATION is what makes the difference. The loop keeps its
// cadence, the failure is logged with a stack, and the pass is reported as
// failed — so the loop's last-success clock (SIGMA-248) goes stale and
// something outside the process can say so. A recovered panic is never silently
// swallowed; it is converted into the same kind of error every other failure in
// that pass produces.
package supervise

import (
	"fmt"
	"log/slog"
	"runtime/debug"
)

// Pass runs one iteration of a background loop, converting a panic into an
// error. The name identifies the loop in the log line.
//
// fn's own defers still run as the stack unwinds — the recover is here, outside
// fn — so anything fn holds (an advisory lock, a transaction rollback) is
// released before this returns.
func Pass(log *slog.Logger, name string, fn func() error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			if log != nil {
				log.Error("background loop panicked — iteration abandoned, loop continues",
					"loop", name, "panic", p, "stack", string(debug.Stack()))
			}
			err = fmt.Errorf("panic in %s: %v", name, p)
		}
	}()
	return fn()
}
