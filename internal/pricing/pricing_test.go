package pricing

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testTable = `{
  "captured_at": "2026-08-09",
  "source": "https://api-docs.deepseek.com/quick_start/pricing",
  "models": {
    "deepseek-v4-flash": {
      "input_cache_hit_per_million_usd": 0.0028,
      "input_cache_miss_per_million_usd": 0.14,
      "output_per_million_usd": 0.28
    }
  }
}`

func writeTestTable(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "prices.json")
	if err := os.WriteFile(path, []byte(testTable), 0o644); err != nil {
		t.Fatalf("write test table: %v", err)
	}
	return path
}

func TestLoad(t *testing.T) {
	path := writeTestTable(t)
	table, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if table.CapturedAt != "2026-08-09" {
		t.Errorf("CapturedAt = %q, want 2026-08-09", table.CapturedAt)
	}
	if _, ok := table.Models["deepseek-v4-flash"]; !ok {
		t.Errorf("Models missing deepseek-v4-flash")
	}
}

func TestLoadMissingCapturedAt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prices.json")
	if err := os.WriteFile(path, []byte(`{"models":{}}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load with no captured_at: want error, got nil")
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load("/nonexistent/prices.json"); err == nil {
		t.Fatal("Load of missing file: want error, got nil")
	}
}

func TestCost(t *testing.T) {
	path := writeTestTable(t)
	table, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// 512 cache-hit tokens, 97 cache-miss tokens, 238 completion tokens —
	// figures drawn from docs/OBSERVED.md's cache table and a real flash run.
	got, tier, err := table.Cost("deepseek-v4-flash", someInstant, 512, 97, 238)
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	if tier != TierFlat {
		t.Errorf("tier = %q, want %q — the test table has no schedule", tier, TierFlat)
	}
	want := float64(512)/1e6*0.0028 + float64(97)/1e6*0.14 + float64(238)/1e6*0.28
	if math.Abs(got-want) > 1e-12 {
		t.Errorf("Cost = %.12f, want %.12f", got, want)
	}
}

func TestCostZeroTokens(t *testing.T) {
	path := writeTestTable(t)
	table, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, _, err := table.Cost("deepseek-v4-flash", someInstant, 0, 0, 0)
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	if got != 0 {
		t.Errorf("Cost of zero tokens = %v, want 0", got)
	}
}

func TestCostUnknownModel(t *testing.T) {
	path := writeTestTable(t)
	table, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, _, err := table.Cost("deepseek-v4-nonexistent", someInstant, 1, 1, 1); err == nil {
		t.Fatal("Cost of unknown model: want error, got nil")
	}
}

// TestRepoTableCarriesGeminiModels pins that the repository's own price
// table keeps the Gemini models the ReviewScreenshot tool costs against,
// each with the per-model source and capture date the brief's convention
// asks for. A typo in the table is the kind of failure that only shows up
// as a silently zero Gemini cost, so it is pinned here instead.
func TestRepoTableCarriesGeminiModels(t *testing.T) {
	table, err := Load("../../configs/prices.json")
	if err != nil {
		t.Fatalf("Load ../../configs/prices.json: %v", err)
	}
	for _, model := range []string{"gemini-3.5-flash", "gemini-3.5-flash-lite", "gemini-3.6-flash", "gemini-3.7-flash"} {
		p, ok := table.Models[model]
		if !ok {
			t.Errorf("table is missing %q", model)
			continue
		}
		if p.Source != "https://ai.google.dev/gemini-api/docs/pricing" {
			t.Errorf("%s source = %q, want Google's pricing page", model, p.Source)
		}
		if p.CapturedAt == "" {
			t.Errorf("%s has no captured_at date", model)
		}
		for _, rate := range []float64{p.InputCacheHitPerMillionUSD, p.InputCacheMissPerMillionUSD, p.OutputPerMillionUSD} {
			if rate <= 0 {
				t.Errorf("%s has a non-positive rate: %v", model, rate)
			}
		}
	}
}

// TestRepoTableGeminiRates pins the exact Gemini rates against Google's
// pricing page, the way TestRepoTableCarriesKimiK3 pins Moonshot's.
//
// The loose test above — a positive rate, a source, a date — is what let
// gemini-3.6-flash sit at its standard $1.50/$7.50 through an introductory
// period billing $0.75/$3.75, over-reporting every 3.6 vision call by a
// factor of two for as long as it was set. A rate that is plausible is not
// a rate that is right, and the only version of this test that would have
// caught it is one that names the numbers.
//
// 3.7 and 3.6 Flash carry the SAME rates on purpose: 3.7 launched at 3.6's
// price. If a future capture makes them differ, that is a real change and
// this test should be updated to say so — not collapsed into one case.
func TestRepoTableGeminiRates(t *testing.T) {
	table, err := Load("../../configs/prices.json")
	if err != nil {
		t.Fatalf("Load ../../configs/prices.json: %v", err)
	}
	for _, tc := range []struct {
		model                 string
		hit, miss, out        float64
		introductoryUntil2027 bool
	}{
		// Introductory through 2026-12-31; these double on 2027-01-01.
		{"gemini-3.7-flash", 0.075, 0.75, 3.75, true},
		{"gemini-3.6-flash", 0.075, 0.75, 3.75, true},
		// No introductory period: these are the standard rates.
		{"gemini-3.5-flash", 0.15, 1.5, 9.0, false},
		{"gemini-3.5-flash-lite", 0.03, 0.3, 2.5, false},
	} {
		t.Run(tc.model, func(t *testing.T) {
			p, ok := table.Models[tc.model]
			if !ok {
				t.Fatalf("table is missing %q", tc.model)
			}
			if p.InputCacheHitPerMillionUSD != tc.hit || p.InputCacheMissPerMillionUSD != tc.miss || p.OutputPerMillionUSD != tc.out {
				t.Errorf("rates = (%v, %v, %v), want (%v, %v, %v)",
					p.InputCacheHitPerMillionUSD, p.InputCacheMissPerMillionUSD, p.OutputPerMillionUSD,
					tc.hit, tc.miss, tc.out)
			}
			// A promotional rate with nothing watching its expiry is the
			// failure this whole test exists for, so the entry has to say
			// so in the table itself rather than only in a commit message.
			if tc.introductoryUntil2027 && !strings.Contains(p.Note, "2027-01-01") {
				t.Errorf("note = %q, want it to name the 2027-01-01 step-up", p.Note)
			}
		})
	}
}

// TestRepoTableCarriesKimiK3 pins the repository's own price table keeping
// kimi-k3 with the exact rates from Moonshot's pricing page — 0.30 cache
// hit, 3.00 cache miss, 15.00 output per million tokens — and its own
// per-model source and capture date (third_party/kimi-docs/pricing/chat-k3.md).
// A typo here shows up as a silently wrong Kimi cost, so it is pinned.
func TestRepoTableCarriesKimiK3(t *testing.T) {
	table, err := Load("../../configs/prices.json")
	if err != nil {
		t.Fatalf("Load ../../configs/prices.json: %v", err)
	}
	p, ok := table.Models["kimi-k3"]
	if !ok {
		t.Fatalf("table is missing kimi-k3")
	}
	if p.InputCacheHitPerMillionUSD != 0.30 || p.InputCacheMissPerMillionUSD != 3.00 || p.OutputPerMillionUSD != 15.00 {
		t.Errorf("kimi-k3 rates = (%v, %v, %v), want (0.30, 3.00, 15.00)",
			p.InputCacheHitPerMillionUSD, p.InputCacheMissPerMillionUSD, p.OutputPerMillionUSD)
	}
	if p.Source != "https://platform.kimi.ai/docs/pricing/chat-k3" {
		t.Errorf("kimi-k3 source = %q, want Moonshot's K3 pricing page", p.Source)
	}
	if p.CapturedAt != "2026-08-13" {
		t.Errorf("kimi-k3 captured_at = %q, want 2026-08-13", p.CapturedAt)
	}
}

// someInstant is an arbitrary time for the tests that predate the clock and
// do not care which one it is — every Cost call needs an instant now, and a
// named constant says "this test is not about the schedule" where a bare
// time.Now() would look like it might be.
var someInstant = time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)

// scheduledTable is the shape configs/prices.json has from 2026-08-16: flat
// rates, plus a schedule that splits the day. The numbers are DeepSeek's own
// (third_party/deepseek-docs/quick_start/pricing.md).
const scheduledTable = `{
  "captured_at": "2026-08-14",
  "source": "https://api-docs.deepseek.com/quick_start/pricing",
  "rate_schedule": {
    "effective_at": "2026-08-16T16:00:00Z",
    "peak_windows_utc": [
      { "from": "01:00", "to": "04:00" },
      { "from": "06:00", "to": "10:00" }
    ],
    "peak": {
      "deepseek-v4-flash": {"input_cache_hit_per_million_usd": 0.014, "input_cache_miss_per_million_usd": 0.44, "output_per_million_usd": 1.32}
    },
    "off_peak": {
      "deepseek-v4-flash": {"input_cache_hit_per_million_usd": 0.007, "input_cache_miss_per_million_usd": 0.22, "output_per_million_usd": 0.66}
    }
  },
  "models": {
    "deepseek-v4-flash": {"input_cache_hit_per_million_usd": 0.0028, "input_cache_miss_per_million_usd": 0.14, "output_per_million_usd": 0.28},
    "gemini-3.7-flash": {"input_cache_hit_per_million_usd": 0.075, "input_cache_miss_per_million_usd": 0.75, "output_per_million_usd": 3.75}
  }
}`

func loadScheduled(t *testing.T) *Table {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "prices.json")
	if err := os.WriteFile(path, []byte(scheduledTable), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	table, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return table
}

func utc(hh, mm int) time.Time {
	return time.Date(2026, 8, 17, hh, mm, 0, 0, time.UTC)
}

// TestTierAtTheHour walks the day. The windows are half-open, so the
// boundary cases are the point: 04:00 is off-peak and 03:59 is not, and a
// closed window would have billed the run at 04:00 sharp at twice the rate.
func TestTierAtTheHour(t *testing.T) {
	table := loadScheduled(t)
	for _, tc := range []struct {
		hh, mm int
		want   Tier
	}{
		{0, 59, TierOffPeak},
		{1, 0, TierPeak}, // the window opens on its From
		{3, 59, TierPeak},
		{4, 0, TierOffPeak}, // and closes before its To
		{5, 30, TierOffPeak},
		{6, 0, TierPeak},
		{9, 59, TierPeak},
		{10, 0, TierOffPeak},
		{23, 59, TierOffPeak},
	} {
		_, tier, err := table.RatesAt("deepseek-v4-flash", utc(tc.hh, tc.mm))
		if err != nil {
			t.Fatalf("%02d:%02d: %v", tc.hh, tc.mm, err)
		}
		if tier != tc.want {
			t.Errorf("%02d:%02dZ tier = %q, want %q", tc.hh, tc.mm, tier, tc.want)
		}
	}
}

// TestTierIsDecidedInUTCWhereverTheCallerIs is the timezone invariant: what a
// token costs is DeepSeek's clock, not the operator's. The same instant
// expressed at UTC+10 must price identically — the alternative is a harness
// that bills differently depending on where it is deployed.
func TestTierIsDecidedInUTCWhereverTheCallerIs(t *testing.T) {
	table := loadScheduled(t)
	brisbane := time.FixedZone("AEST", 10*60*60)

	// 02:00 UTC is peak. At UTC+10 that same instant reads 12:00 the next
	// day — a local noon, which is nowhere near any window as written.
	instant := utc(2, 0)
	local := instant.In(brisbane)
	if local.Hour() != 12 {
		t.Fatalf("precondition: 02:00Z at +10 should read 12:00, got %02d:%02d", local.Hour(), local.Minute())
	}
	_, tierUTC, err := table.RatesAt("deepseek-v4-flash", instant)
	if err != nil {
		t.Fatal(err)
	}
	_, tierLocal, err := table.RatesAt("deepseek-v4-flash", local)
	if err != nil {
		t.Fatal(err)
	}
	if tierUTC != TierPeak || tierLocal != TierPeak {
		t.Errorf("tiers = (%q, %q), want both %q — the same instant must price the same however it is expressed", tierUTC, tierLocal, TierPeak)
	}
}

// TestFlatUntilEffectiveAt pins the switchover. A run the minute before the
// split bills the old flat rate; a run the minute after bills by the hour.
func TestFlatUntilEffectiveAt(t *testing.T) {
	table := loadScheduled(t)
	effective := time.Date(2026, 8, 16, 16, 0, 0, 0, time.UTC)

	// 15:59Z on the 16th is outside any peak window anyway, so a bug that
	// ignored effective_at would still say off_peak here — which is why the
	// assertion is on the tier being flat, not on the number.
	_, tier, err := table.RatesAt("deepseek-v4-flash", effective.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if tier != TierFlat {
		t.Errorf("a minute before effective_at: tier = %q, want %q", tier, TierFlat)
	}
	p, tier, err := table.RatesAt("deepseek-v4-flash", effective)
	if err != nil {
		t.Fatal(err)
	}
	if tier != TierOffPeak {
		t.Errorf("at effective_at exactly: tier = %q, want %q", tier, TierOffPeak)
	}
	if p.OutputPerMillionUSD != 0.66 {
		t.Errorf("output rate = %v, want the off-peak 0.66", p.OutputPerMillionUSD)
	}
}

// TestCostAcrossTheSplit is the figure a user would notice: the same tokens,
// the same model, three prices depending only on when they were spent.
func TestCostAcrossTheSplit(t *testing.T) {
	table := loadScheduled(t)
	const hit, miss, out = 100_000, 10_000, 5_000

	before, _, err := table.Cost("deepseek-v4-flash", time.Date(2026, 8, 15, 2, 0, 0, 0, time.UTC), hit, miss, out)
	if err != nil {
		t.Fatal(err)
	}
	offPeak, _, err := table.Cost("deepseek-v4-flash", utc(12, 0), hit, miss, out)
	if err != nil {
		t.Fatal(err)
	}
	peak, _, err := table.Cost("deepseek-v4-flash", utc(2, 0), hit, miss, out)
	if err != nil {
		t.Fatal(err)
	}
	if !(before < offPeak && offPeak < peak) {
		t.Errorf("costs = (before %.6f, off-peak %.6f, peak %.6f), want strictly increasing", before, offPeak, peak)
	}
	// Off-peak is exactly half of peak on every rate, so the totals are too.
	if math.Abs(peak-2*offPeak) > 1e-12 {
		t.Errorf("peak %.9f is not twice off-peak %.9f", peak, offPeak)
	}
}

// TestModelOutsideTheScheduleStaysFlat: Gemini bills one rate around the
// clock and appears in neither half of the schedule, so a vision call at
// 02:00 UTC must not be charged a DeepSeek peak rate — or, worse, fail to
// price at all.
func TestModelOutsideTheScheduleStaysFlat(t *testing.T) {
	table := loadScheduled(t)
	p, tier, err := table.RatesAt("gemini-3.7-flash", utc(2, 0))
	if err != nil {
		t.Fatal(err)
	}
	if tier != TierFlat {
		t.Errorf("tier = %q, want %q", tier, TierFlat)
	}
	if p.OutputPerMillionUSD != 3.75 {
		t.Errorf("output rate = %v, want Gemini's flat 3.75", p.OutputPerMillionUSD)
	}
}

// TestZeroTimeIsRefused. A caller that forgot the instant would otherwise
// compare as before every effective_at and quietly bill at the pre-split
// flat rate for ever — a quarter of the truth at peak, and nothing anywhere
// would look wrong.
func TestZeroTimeIsRefused(t *testing.T) {
	table := loadScheduled(t)
	if _, _, err := table.Cost("deepseek-v4-flash", time.Time{}, 1, 1, 1); err == nil {
		t.Fatal("Cost with the zero time: want an error, got nil")
	}
}

// TestLocalWindows is the operator-facing half, and the only place a
// timezone other than UTC appears. At UTC+10 DeepSeek's two peak windows
// land at 11:00-14:00 and 16:00-20:00 — the middle of an Australian working
// day, which is the fact the rendering exists to make visible.
func TestLocalWindows(t *testing.T) {
	table := loadScheduled(t)
	got := table.Schedule.LocalWindows(time.FixedZone("AEST", 10*60*60))
	want := []LocalWindow{
		{From: "11:00", To: "14:00"},
		{From: "16:00", To: "20:00"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d windows, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].From != want[i].From || got[i].To != want[i].To || got[i].Crossed {
			t.Errorf("window %d = %s-%s (crossed %v), want %s-%s (crossed false)",
				i, got[i].From, got[i].To, got[i].Crossed, want[i].From, want[i].To)
		}
	}
}

// TestLocalWindowsFlagACrossedDay. A window that lands on the next day where
// the reader is must say so, or "22:00-02:00" reads as an ordinary evening.
func TestLocalWindowsFlagACrossedDay(t *testing.T) {
	s := &RateSchedule{
		EffectiveAt:    time.Date(2026, 8, 16, 16, 0, 0, 0, time.UTC),
		PeakWindowsUTC: []Window{{From: "06:00", To: "10:00"}},
		Peak:           map[string]ModelPrices{"m": {}},
		OffPeak:        map[string]ModelPrices{"m": {}},
	}
	if err := s.normalise(); err != nil {
		t.Fatal(err)
	}
	// At UTC-8, 06:00-10:00Z is 22:00 the previous day to 02:00.
	got := s.LocalWindows(time.FixedZone("PST", -8*60*60))
	if len(got) != 1 {
		t.Fatalf("got %d windows, want 1", len(got))
	}
	if got[0].From != "22:00" || got[0].To != "02:00" || !got[0].Crossed {
		t.Errorf("window = %s-%s (crossed %v), want 22:00-02:00 (crossed true)", got[0].From, got[0].To, got[0].Crossed)
	}
}

// TestScheduleValidation. Every one of these produces a table that loads
// fine and prices everything off-peak for ever, which is why they are load
// errors rather than something to notice later.
func TestScheduleValidation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		schedule string
	}{
		{"no windows", `"effective_at":"2026-08-16T16:00:00Z","peak_windows_utc":[],"peak":{"m":{}},"off_peak":{"m":{}}`},
		{"malformed window", `"effective_at":"2026-08-16T16:00:00Z","peak_windows_utc":[{"from":"1am","to":"04:00"}],"peak":{"m":{}},"off_peak":{"m":{}}`},
		{"hour out of range", `"effective_at":"2026-08-16T16:00:00Z","peak_windows_utc":[{"from":"25:00","to":"04:00"}],"peak":{"m":{}},"off_peak":{"m":{}}`},
		{"empty window", `"effective_at":"2026-08-16T16:00:00Z","peak_windows_utc":[{"from":"01:00","to":"01:00"}],"peak":{"m":{}},"off_peak":{"m":{}}`},
		{"no effective_at", `"peak_windows_utc":[{"from":"01:00","to":"04:00"}],"peak":{"m":{}},"off_peak":{"m":{}}`},
		{"peak only", `"effective_at":"2026-08-16T16:00:00Z","peak_windows_utc":[{"from":"01:00","to":"04:00"}],"peak":{"m":{}},"off_peak":{}`},
		{"model priced at peak only", `"effective_at":"2026-08-16T16:00:00Z","peak_windows_utc":[{"from":"01:00","to":"04:00"}],"peak":{"m":{},"n":{}},"off_peak":{"m":{}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "prices.json")
			body := `{"captured_at":"2026-08-14","models":{"m":{}},"rate_schedule":{` + tc.schedule + `}}`
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Error("want a load error, got nil — this table would price every hour off-peak")
			}
		})
	}
}

// TestRepoTableSchedule pins the shipped schedule against DeepSeek's own
// page: the effective instant, the two windows, and that both halves price
// every DeepSeek model the table carries. A model the flat table has and the
// schedule does not would keep billing its pre-split rate after the split,
// which is between a half and a quarter of the truth.
func TestRepoTableSchedule(t *testing.T) {
	table, err := Load("../../configs/prices.json")
	if err != nil {
		t.Fatalf("Load ../../configs/prices.json: %v", err)
	}
	s := table.Schedule
	if s == nil {
		t.Fatal("the shipped table has no rate_schedule")
	}
	if want := time.Date(2026, 8, 16, 16, 0, 0, 0, time.UTC); !s.EffectiveAt.Equal(want) {
		t.Errorf("effective_at = %s, want %s", s.EffectiveAt, want)
	}
	if len(s.PeakWindowsUTC) != 2 ||
		s.PeakWindowsUTC[0].From != "01:00" || s.PeakWindowsUTC[0].To != "04:00" ||
		s.PeakWindowsUTC[1].From != "06:00" || s.PeakWindowsUTC[1].To != "10:00" {
		t.Errorf("peak windows = %+v, want 01:00-04:00 and 06:00-10:00 UTC", s.PeakWindowsUTC)
	}
	for model := range table.Models {
		if !strings.HasPrefix(model, "deepseek-") {
			continue
		}
		if _, ok := s.Peak[model]; !ok {
			t.Errorf("%s is in the flat table but not in the schedule's peak rates", model)
		}
		if _, ok := s.OffPeak[model]; !ok {
			t.Errorf("%s is in the flat table but not in the schedule's off-peak rates", model)
		}
	}
	// Off-peak is half of peak, which is DeepSeek's own statement about the
	// split and the cheapest possible check that a transcription slipped.
	for model, peak := range s.Peak {
		off := s.OffPeak[model]
		for _, pair := range [][2]float64{
			{peak.InputCacheHitPerMillionUSD, off.InputCacheHitPerMillionUSD},
			{peak.InputCacheMissPerMillionUSD, off.InputCacheMissPerMillionUSD},
			{peak.OutputPerMillionUSD, off.OutputPerMillionUSD},
		} {
			if math.Abs(pair[0]/2-pair[1]) > 1e-12 {
				t.Errorf("%s: off-peak %v is not half of peak %v", model, pair[1], pair[0])
			}
		}
	}
}
