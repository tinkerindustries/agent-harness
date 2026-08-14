// Package pricing loads the DeepSeek price table from config and computes
// cost from token counts. Rates change on DeepSeek's schedule, not the
// harness's, so they are never compiled in (docs/DESIGN.md §4.9) — and from
// 2026-08-16 that includes *when* a rate applies, not just what it is, so
// the peak windows are data here too rather than constants in Go.
//
// One thing in here is deliberately not configurable: the windows are UTC,
// and a cost is computed against UTC, because that is the clock DeepSeek
// bills on. An operator's own timezone changes nothing about what a token
// costs. It changes only which of their working hours are expensive, which
// is a question about display — LocalWindows answers it, and nothing on the
// costing path consults it.
package pricing

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// ModelPrices are USD rates per one million tokens for a single model.
// Source and CapturedAt are optional per-model provenance for rates that
// come from a different page or date than the table's own top-level fields
// — the Gemini entries, whose prices come from Google's pricing page while
// the table's top-level source names DeepSeek's. Absent per model, a cost
// readout falls back to the table's top-level date.
//
// Note is a dated warning about the rate itself, for the case the Gemini
// entries are in: a price that is correct today because it is promotional,
// and that steps up on a known date with nothing in this package watching
// for it. It is a real field rather than a comment because JSON has no
// comments and an unknown key here is silently dropped — which is exactly
// how gemini-3.6-flash sat at its standard rate through an introductory
// period and over-reported every vision call by a factor of two.
type ModelPrices struct {
	InputCacheHitPerMillionUSD  float64 `json:"input_cache_hit_per_million_usd"`
	InputCacheMissPerMillionUSD float64 `json:"input_cache_miss_per_million_usd"`
	OutputPerMillionUSD         float64 `json:"output_per_million_usd"`
	Source                      string  `json:"source,omitempty"`
	CapturedAt                  string  `json:"captured_at,omitempty"`
	Note                        string  `json:"note,omitempty"`
}

// Tier names which set of rates a cost was computed at, so a figure can say
// why it is what it is rather than leaving a reader to work out whether a
// dear sub-turn was a long one or a badly timed one.
type Tier string

const (
	// TierFlat is one rate around the clock: every model before a schedule's
	// effective_at, and every model a schedule does not cover — Gemini and
	// Kimi bill one rate whatever the hour.
	TierFlat    Tier = "flat"
	TierPeak    Tier = "peak"
	TierOffPeak Tier = "off_peak"
)

// Window is one span of the day in UTC, half-open: From is inside it and To
// is not, so back-to-back windows do not both claim the instant they meet.
// Both are "HH:MM". A window whose To is before its From wraps midnight.
type Window struct {
	From string `json:"from"`
	To   string `json:"to"`

	// Minutes from midnight, parsed once by Load so a cost lookup is
	// arithmetic rather than string parsing.
	fromMin int
	toMin   int
}

func (w Window) contains(minuteOfDay int) bool {
	if w.fromMin <= w.toMin {
		return minuteOfDay >= w.fromMin && minuteOfDay < w.toMin
	}
	// Wraps midnight. Nothing in DeepSeek's schedule does today; it is here
	// because the alternative to handling it is mispricing every token in
	// such a window, silently, and the handling is one line.
	return minuteOfDay >= w.fromMin || minuteOfDay < w.toMin
}

// RateSchedule is rates that depend on the time of day, from a date.
//
// DeepSeek splits billing into peak and off-peak at 16:00 UTC on 2026-08-16,
// with off-peak at half the peak rate. Before EffectiveAt the table's flat
// Models rates apply; from it, a model listed in Peak and OffPeak is priced
// by the hour its request was made in. A model in neither — every Gemini and
// Kimi entry — stays on its flat rate, because those providers do not do
// this.
//
// The windows are data, not constants, for the reason the rates are: they
// are DeepSeek's to change, and a schedule change that needed a Go release
// to take effect would mean the harness bills its own figures wrong until
// someone noticed.
type RateSchedule struct {
	EffectiveAt    time.Time              `json:"effective_at"`
	PeakWindowsUTC []Window               `json:"peak_windows_utc"`
	Note           string                 `json:"note,omitempty"`
	Peak           map[string]ModelPrices `json:"peak"`
	OffPeak        map[string]ModelPrices `json:"off_peak"`
}

// IsPeak answers whether at falls in one of the peak windows. The windows
// are UTC because DeepSeek states them in UTC; at is converted rather than
// assumed, so a caller holding a local time gets the right answer instead of
// a plausible one.
func (s *RateSchedule) IsPeak(at time.Time) bool {
	u := at.UTC()
	minuteOfDay := u.Hour()*60 + u.Minute()
	for _, w := range s.PeakWindowsUTC {
		if w.contains(minuteOfDay) {
			return true
		}
	}
	return false
}

// LocalWindow is a peak window rendered in some other zone, for a person to
// read. Crossed is true when the window lands on a different date there than
// it does in UTC — 06:00–10:00 UTC is 16:00–20:00 the same day at +10, but
// 22:00–02:00 UTC would not be, and a range printed without that said reads
// as an ordinary evening rather than one spanning midnight.
type LocalWindow struct {
	From    string
	To      string
	Crossed bool
}

// LocalWindows renders the peak windows in loc, for display only. Nothing on
// the costing path calls this: what a token costs depends on DeepSeek's
// clock, not on where the operator happens to be sitting. What depends on
// where they are sitting is whether the expensive hours land in the middle
// of their working day — at UTC+10 the two windows are 11:00–14:00 and
// 16:00–20:00, which is most of one — and that is worth being able to print.
//
// A nil loc means time.Local, the zone of the machine the harness runs on.
func (s *RateSchedule) LocalWindows(loc *time.Location) []LocalWindow {
	if loc == nil {
		loc = time.Local
	}
	// Rendered against EffectiveAt's own date so a zone with daylight saving
	// resolves to the offset actually in force then, rather than to
	// whichever offset today happens to have.
	day := s.EffectiveAt.UTC()
	out := make([]LocalWindow, 0, len(s.PeakWindowsUTC))
	for _, w := range s.PeakWindowsUTC {
		from := time.Date(day.Year(), day.Month(), day.Day(), w.fromMin/60, w.fromMin%60, 0, 0, time.UTC).In(loc)
		toDay := day
		if w.toMin <= w.fromMin {
			toDay = day.AddDate(0, 0, 1) // the window wraps midnight in UTC
		}
		to := time.Date(toDay.Year(), toDay.Month(), toDay.Day(), w.toMin/60, w.toMin%60, 0, 0, time.UTC).In(loc)
		out = append(out, LocalWindow{
			From:    from.Format("15:04"),
			To:      to.Format("15:04"),
			Crossed: from.Day() != to.Day(),
		})
	}
	return out
}

// Table is a price table read from config. CapturedAt records when the
// rates were checked against the live pricing page, so a cost readout can
// show its own age rather than implying current accuracy it doesn't have.
type Table struct {
	CapturedAt string                 `json:"captured_at"`
	Source     string                 `json:"source"`
	Models     map[string]ModelPrices `json:"models"`
	// Schedule is optional: a table without one prices everything flat.
	Schedule *RateSchedule `json:"rate_schedule,omitempty"`
}

// Load reads and parses a price table from path.
func Load(path string) (*Table, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("pricing: read %s: %w", path, err)
	}
	var t Table
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("pricing: parse %s: %w", path, err)
	}
	if t.CapturedAt == "" {
		return nil, fmt.Errorf("pricing: %s has no captured_at date", path)
	}
	if t.Schedule != nil {
		if err := t.Schedule.normalise(); err != nil {
			return nil, fmt.Errorf("pricing: %s: %w", path, err)
		}
	}
	return &t, nil
}

// normalise validates the schedule and parses its windows into minutes.
//
// Every failure here is loud, and deliberately so: the failure mode of a
// quietly bad schedule is that no instant ever matches a peak window, so
// every token in the day bills at the off-peak rate and the total simply
// looks a bit low. Nothing downstream can tell that from a cheap week.
func (s *RateSchedule) normalise() error {
	if s.EffectiveAt.IsZero() {
		return fmt.Errorf("rate_schedule has no effective_at")
	}
	if len(s.PeakWindowsUTC) == 0 {
		return fmt.Errorf("rate_schedule has no peak_windows_utc: with none, every hour would price off-peak")
	}
	for i := range s.PeakWindowsUTC {
		w := &s.PeakWindowsUTC[i]
		from, err := parseHHMM(w.From)
		if err != nil {
			return fmt.Errorf("rate_schedule peak window %d: from: %w", i, err)
		}
		to, err := parseHHMM(w.To)
		if err != nil {
			return fmt.Errorf("rate_schedule peak window %d: to: %w", i, err)
		}
		if from == to {
			return fmt.Errorf("rate_schedule peak window %d is empty: from and to are both %s", i, w.From)
		}
		w.fromMin, w.toMin = from, to
	}
	if len(s.Peak) == 0 || len(s.OffPeak) == 0 {
		return fmt.Errorf("rate_schedule needs both peak and off_peak rates")
	}
	// A model priced in one half and not the other silently falls back to
	// its flat rate for half the day, which is the kind of wrong that looks
	// right. Require the two halves to name the same models.
	for model := range s.Peak {
		if _, ok := s.OffPeak[model]; !ok {
			return fmt.Errorf("rate_schedule prices %q at peak but not off-peak", model)
		}
	}
	for model := range s.OffPeak {
		if _, ok := s.Peak[model]; !ok {
			return fmt.Errorf("rate_schedule prices %q off-peak but not at peak", model)
		}
	}
	return nil
}

func parseHHMM(s string) (int, error) {
	h, m, ok := strings.Cut(s, ":")
	if !ok {
		return 0, fmt.Errorf("%q is not HH:MM", s)
	}
	hh, err := strconv.Atoi(h)
	if err != nil || hh < 0 || hh > 23 {
		return 0, fmt.Errorf("%q has no valid hour", s)
	}
	mm, err := strconv.Atoi(m)
	if err != nil || mm < 0 || mm > 59 {
		return 0, fmt.Errorf("%q has no valid minute", s)
	}
	return hh*60 + mm, nil
}

// RatesAt returns the rates in force for model at the instant at, and which
// tier they came from.
func (t *Table) RatesAt(model string, at time.Time) (ModelPrices, Tier, error) {
	flat, ok := t.Models[model]
	if !ok {
		return ModelPrices{}, "", fmt.Errorf("pricing: no entry for model %q in table captured %s", model, t.CapturedAt)
	}
	// A zero time is a caller that forgot to pass one, not a caller asking
	// about the epoch. Left alone it would compare as before every
	// effective_at and quietly price at the pre-split flat rate for ever,
	// which after 2026-08-16 is between a half and a quarter of the truth.
	if at.IsZero() {
		return ModelPrices{}, "", fmt.Errorf("pricing: cost of %q needs the instant the tokens were billed, got the zero time", model)
	}
	s := t.Schedule
	if s == nil || at.Before(s.EffectiveAt) {
		return flat, TierFlat, nil
	}
	if s.IsPeak(at) {
		if p, ok := s.Peak[model]; ok {
			return p, TierPeak, nil
		}
	} else if p, ok := s.OffPeak[model]; ok {
		return p, TierOffPeak, nil
	}
	// Not a model the schedule covers: Gemini and Kimi bill one rate around
	// the clock.
	return flat, TierFlat, nil
}

// Cost computes USD cost from token counts for model, at the rates in force
// at the instant `at`, and reports which tier it used. completionTokens is
// the total output token count returned by the API; reasoning tokens and
// answer tokens bill at the same output rate, so reasoning is not charged
// separately from completionTokens.
//
// `at` is required rather than defaulted to time.Now() because the two are
// not the same instant and the difference is billable: a sub-turn that
// starts at 03:58 UTC and returns at 04:03 crosses out of a peak window
// while it runs. The caller is the only thing that knows which instant it
// means — internal/session passes the moment the request was sent, which is
// when the tokens were submitted for billing.
func (t *Table) Cost(model string, at time.Time, cacheHitTokens, cacheMissTokens, completionTokens int) (float64, Tier, error) {
	p, tier, err := t.RatesAt(model, at)
	if err != nil {
		return 0, "", err
	}
	cost := float64(cacheHitTokens)/1e6*p.InputCacheHitPerMillionUSD +
		float64(cacheMissTokens)/1e6*p.InputCacheMissPerMillionUSD +
		float64(completionTokens)/1e6*p.OutputPerMillionUSD
	return cost, tier, nil
}
