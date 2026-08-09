package queue

import (
	"sync"
	"time"
)

// ProgressLimiter rate-limits progress publication to at most once per
// interval, per run (docs/DESIGN.md §4.10: "rate-limited to at most one
// message per second"). One limiter belongs to one request; the worker
// pool creates a fresh one per run rather than sharing it, the same reason
// the churn diagnostic's state is per-session (docs/CACHE.md).
type ProgressLimiter struct {
	interval time.Duration

	mu   sync.Mutex
	last time.Time
}

// NewProgressLimiter returns a limiter that allows one call per interval.
func NewProgressLimiter(interval time.Duration) *ProgressLimiter {
	return &ProgressLimiter{interval: interval}
}

// Allow reports whether a progress message may be sent at now, and if so
// records now as the last allowed time. The first call always allows,
// since there is no prior progress message to rate-limit against.
func (p *ProgressLimiter) Allow(now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.last.IsZero() && now.Sub(p.last) < p.interval {
		return false
	}
	p.last = now
	return true
}
