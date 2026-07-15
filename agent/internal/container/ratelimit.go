package container

import (
	"sync"
	"time"
)

// rateLimiter is a simple token-bucket bounding how many container ops the
// agent will apply per unit time, regardless of DSD contents. It lives on the
// agent (moved here from P1-2's delivery layer, per the architecture) so a
// runaway or malicious DSD cannot drive unbounded Docker churn on the host.
type rateLimiter struct {
	mu       sync.Mutex
	tokens   float64
	max      float64
	perSec   float64
	lastFill time.Time
	now      func() time.Time
}

func newRateLimiter(burst int, perSec float64) *rateLimiter {
	return &rateLimiter{
		tokens:   float64(burst),
		max:      float64(burst),
		perSec:   perSec,
		lastFill: time.Now(),
		now:      time.Now,
	}
}

// allow consumes one token, refilling by elapsed time first. Returns false when
// the bucket is empty, in which case the caller fails the op (the reconcile
// loop and next resync will retry, so this throttles rather than drops).
func (r *rateLimiter) allow() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	elapsed := now.Sub(r.lastFill).Seconds()
	if elapsed > 0 {
		r.tokens += elapsed * r.perSec
		if r.tokens > r.max {
			r.tokens = r.max
		}
		r.lastFill = now
	}
	if r.tokens < 1 {
		return false
	}
	r.tokens--
	return true
}
