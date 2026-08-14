package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mrgeoffrich/deepseek-harness/internal/pricing"
)

// The ask command's two pricing lines. They are the only place in the
// harness that renders DeepSeek's peak windows for a person to read, and the
// only place an operator's own timezone appears at all — internal/pricing
// costs in UTC and says why. What is worth pinning here is that the local
// rendering is genuinely local: the point of the line is that somebody at
// UTC+10 learns DeepSeek's expensive hours are the middle of their working
// day, and a line that printed the UTC pair twice would look right.

func scheduledTestTable(t *testing.T) *pricing.Table {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/prices.json"
	body := `{
	  "captured_at": "2026-08-14",
	  "rate_schedule": {
	    "effective_at": "2026-08-16T16:00:00Z",
	    "peak_windows_utc": [{"from":"01:00","to":"04:00"},{"from":"06:00","to":"10:00"}],
	    "peak": {"deepseek-v4-flash": {"input_cache_hit_per_million_usd":0.014,"input_cache_miss_per_million_usd":0.44,"output_per_million_usd":1.32}},
	    "off_peak": {"deepseek-v4-flash": {"input_cache_hit_per_million_usd":0.007,"input_cache_miss_per_million_usd":0.22,"output_per_million_usd":0.66}}
	  },
	  "models": {"deepseek-v4-flash": {"input_cache_hit_per_million_usd":0.0028,"input_cache_miss_per_million_usd":0.14,"output_per_million_usd":0.28}}
	}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	table, err := pricing.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return table
}

// withLocalZone points time.Local at loc for the duration of the test. The
// helper under test reads time.Local by design — it is asking "where is this
// harness running" — so the only way to test it is to answer that question.
func withLocalZone(t *testing.T, loc *time.Location) {
	t.Helper()
	saved := time.Local
	time.Local = loc
	t.Cleanup(func() { time.Local = saved })
}

func TestPeakWindowLineIsLocal(t *testing.T) {
	withLocalZone(t, time.FixedZone("AEST", 10*60*60))
	table := scheduledTestTable(t)

	// After the split, so no "from ..." tail.
	line := peakWindowLine(table, time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC))
	// The whole point: DeepSeek's 01:00-04:00 and 06:00-10:00 UTC are the
	// middle of the working day at +10.
	for _, want := range []string{"11:00-14:00", "16:00-20:00", "AEST"} {
		if !strings.Contains(line, want) {
			t.Errorf("line = %q, want it to contain %q", line, want)
		}
	}
	// The UTC pair rides along, because that is what DeepSeek's pricing page
	// says and a reader checking one against the other should not have to
	// convert in their head.
	if !strings.Contains(line, "01:00-04:00, 06:00-10:00 UTC") {
		t.Errorf("line = %q, want the UTC windows alongside the local ones", line)
	}
	if strings.Contains(line, "from ") {
		t.Errorf("line = %q, should not announce a future date once the split is in force", line)
	}
}

func TestPeakWindowLineAnnouncesAPendingSplit(t *testing.T) {
	withLocalZone(t, time.FixedZone("AEST", 10*60*60))
	table := scheduledTestTable(t)

	// Before effective_at: the windows are real but not yet charged, and the
	// line has to say so or it reads as a bill already being paid.
	line := peakWindowLine(table, time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC))
	if !strings.Contains(line, "from ") {
		t.Errorf("line = %q, want it to name the date the split starts", line)
	}
	// 16:00Z on the 16th is 02:00 on the 17th at +10 — rendered in the
	// reader's zone, like everything else on this line.
	if !strings.Contains(line, "2026-08-17 02:00") {
		t.Errorf("line = %q, want the effective date in local time", line)
	}
}

func TestPeakWindowLineEmptyWithoutASchedule(t *testing.T) {
	if got := peakWindowLine(nil, time.Now()); got != "" {
		t.Errorf("nil table: got %q, want \"\"", got)
	}
	if got := peakWindowLine(&pricing.Table{CapturedAt: "2026-08-14"}, time.Now()); got != "" {
		t.Errorf("table with no schedule: got %q, want \"\"", got)
	}
}

func TestRateTierNote(t *testing.T) {
	// Flat says nothing: with no other tier to have been on, the note would
	// be noise on every run of every provider that never had a split.
	for tier, want := range map[pricing.Tier]string{
		pricing.TierFlat:    "",
		pricing.TierPeak:    ", peak rate",
		pricing.TierOffPeak: ", off-peak rate",
		pricing.Tier(""):    "",
	} {
		if got := rateTierNote(tier); got != want {
			t.Errorf("rateTierNote(%q) = %q, want %q", tier, got, want)
		}
	}
}
