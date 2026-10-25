package providerhttp

import (
	"log"
	"time"
)

// StallThreshold is the quiet stretch worth a line on stderr: a provider
// taking this long to send a response's headers, or a started stream going
// this long between two lines. Shorter gaps are ordinary model latency.
// These lines are how a parent tells a stalled connection from anything
// downstream of this process, since nothing else records when a frame
// arrived: the event log stamps a whole sub-turn when it commits.
const StallThreshold = 10 * time.Second

// logf is where the stall and retry lines go; a test swaps it.
var logf = log.Printf

// GapLog watches one stream's lines and logs a gap of StallThreshold or
// more once the line that ends it arrives, so the line names the gap's full
// length. A gap the idle watchdog ends is not logged here: that ends the
// stream with its own error.
type GapLog struct {
	label string
	now   func() time.Time
	start time.Time
	last  time.Time
	lines int
	bytes int64
}

// NewGapLog starts watching a stream whose body has just been handed over.
// label names the provider in the line, the way ErrPrefix does in errors.
func NewGapLog(label string) *GapLog {
	return newGapLog(label, time.Now)
}

func newGapLog(label string, now func() time.Time) *GapLog {
	t := now()
	return &GapLog{label: label, now: now, start: t, last: t}
}

// Line records one line read off the stream, of n bytes.
func (g *GapLog) Line(n int) {
	t := g.now()
	if gap := t.Sub(g.last); gap >= StallThreshold {
		logf("%s: stream quiet for %.1fs, %.1fs in, after %d lines (%d bytes); data arriving again", g.label, gap.Seconds(), g.last.Sub(g.start).Seconds(), g.lines, g.bytes)
	}
	g.last = t
	g.lines++
	g.bytes += int64(n)
}
