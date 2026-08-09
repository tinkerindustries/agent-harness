package queue

import (
	"testing"
	"time"
)

func TestProgressLimiterAllowsFirstCall(t *testing.T) {
	l := NewProgressLimiter(time.Second)
	if !l.Allow(time.Now()) {
		t.Fatal("expected the first call to be allowed")
	}
}

func TestProgressLimiterSuppressesWithinInterval(t *testing.T) {
	l := NewProgressLimiter(time.Second)
	now := time.Now()
	if !l.Allow(now) {
		t.Fatal("expected the first call to be allowed")
	}
	if l.Allow(now.Add(500 * time.Millisecond)) {
		t.Fatal("expected a call inside the interval to be suppressed")
	}
	if !l.Allow(now.Add(1100 * time.Millisecond)) {
		t.Fatal("expected a call past the interval to be allowed")
	}
}

func TestProgressLimiterConcurrentCallsAllowExactlyOnePerInterval(t *testing.T) {
	l := NewProgressLimiter(50 * time.Millisecond)
	now := time.Now()

	const n = 50
	results := make(chan bool, n)
	for i := 0; i < n; i++ {
		go func() { results <- l.Allow(now) }()
	}
	allowed := 0
	for i := 0; i < n; i++ {
		if <-results {
			allowed++
		}
	}
	if allowed != 1 {
		t.Fatalf("expected exactly 1 allowed call among %d concurrent calls at the same instant, got %d", n, allowed)
	}
}
