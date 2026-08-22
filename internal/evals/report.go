package evals

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

// A Summary is one metric under one variant, across every run of it.
type Summary struct {
	Metric  string  `json:"metric"`
	Variant string  `json:"variant"`
	N       int     `json:"n"`
	Mean    float64 `json:"mean"`
	// StdDev is the sample standard deviation, and Stderr the standard error
	// of the mean. Both are reported because a difference in means says
	// nothing on its own: with runs this few, the spread is usually the story.
	StdDev float64 `json:"stddev"`
	Stderr float64 `json:"stderr"`
}

// ReportFromStore rebuilds a Report from the rows an eval wrote, so a caller
// that reads the store after the fact — the HTTP surface, following a run it
// did not orchestrate — gets its table from the same code that built the
// original.
func ReportFromStore(run store.EvalRun, members []store.EvalMember) *Report {
	report := &Report{
		EvalRunID: run.ID, Suite: run.Suite,
		Variants: run.Variants, Replicates: run.Replicates, StartedAt: run.StartedAt,
	}
	for _, m := range members {
		r := Run{
			TaskID: m.TaskID, Variant: m.Variant, Replicate: m.Replicate,
			RequestID: m.RequestID, SessionID: m.SessionID, Status: m.Status,
			CostUSD: m.CostUSD, SubTurns: m.SubTurns, Err: m.Error,
		}
		if len(m.Scores) > 0 {
			_ = json.Unmarshal(m.Scores, &r.Scores)
		}
		if len(m.Verdict) > 0 {
			var v Verdict
			if json.Unmarshal(m.Verdict, &v) == nil {
				r.Verdict = &v
			}
		}
		report.Runs = append(report.Runs, r)
	}
	return report
}

// Summarise reduces a report to one row per (metric, variant).
func Summarise(r *Report) []Summary {
	type key struct{ metric, variant string }
	values := map[key][]float64{}
	for _, run := range r.Runs {
		for name, v := range run.Scores {
			values[key{name, run.Variant}] = append(values[key{name, run.Variant}], v)
		}
		if run.Verdict != nil {
			values[key{"judge_score", run.Variant}] = append(values[key{"judge_score", run.Variant}], float64(run.Verdict.Score))
			completed := 0.0
			if run.Verdict.Completed {
				completed = 1
			}
			values[key{"judge_completed", run.Variant}] = append(values[key{"judge_completed", run.Variant}], completed)
		}
		if run.CostUSD > 0 {
			values[key{"cost_usd", run.Variant}] = append(values[key{"cost_usd", run.Variant}], run.CostUSD)
		}
		if run.SubTurns > 0 {
			values[key{"sub_turns", run.Variant}] = append(values[key{"sub_turns", run.Variant}], float64(run.SubTurns))
		}
	}

	var out []Summary
	for k, vs := range values {
		mean, sd := meanAndStdDev(vs)
		stderr := 0.0
		if len(vs) > 1 {
			stderr = sd / math.Sqrt(float64(len(vs)))
		}
		out = append(out, Summary{Metric: k.metric, Variant: k.variant, N: len(vs), Mean: mean, StdDev: sd, Stderr: stderr})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Metric != out[j].Metric {
			return metricOrder(out[i].Metric) < metricOrder(out[j].Metric)
		}
		return out[i].Variant < out[j].Variant
	})
	return out
}

// reportedMetrics is the print order: the mechanical counters first, then the
// judge, then what the run cost.
var reportedMetrics = []string{
	"search_via_tool", "search_via_tool_decay", "searches_total",
	"tool_error_rate", "read_before_edit_misses", "edit_miss_rate_decay",
	"tool_calls", "context_tokens_max",
	"judge_score", "judge_completed",
	"sub_turns", "cost_usd",
}

func metricOrder(name string) int {
	for i, m := range reportedMetrics {
		if m == name {
			return i
		}
	}
	return len(reportedMetrics)
}

func meanAndStdDev(vs []float64) (float64, float64) {
	if len(vs) == 0 {
		return 0, 0
	}
	var sum float64
	for _, v := range vs {
		sum += v
	}
	mean := sum / float64(len(vs))
	if len(vs) < 2 {
		return mean, 0
	}
	var sq float64
	for _, v := range vs {
		sq += (v - mean) * (v - mean)
	}
	return mean, math.Sqrt(sq / float64(len(vs)-1))
}

// WriteTable prints the comparison: one row per metric, one column per
// variant, each cell a mean with its standard error. The baseline is the
// first variant named.
func WriteTable(w io.Writer, r *Report) {
	summaries := Summarise(r)
	byMetric := map[string]map[string]Summary{}
	var order []string
	for _, s := range summaries {
		if _, ok := byMetric[s.Metric]; !ok {
			byMetric[s.Metric] = map[string]Summary{}
			order = append(order, s.Metric)
		}
		byMetric[s.Metric][s.Variant] = s
	}

	failed, capped := 0, 0
	for _, run := range r.Runs {
		if run.Err != "" {
			failed++
		}
		if run.Status == "max_turns" {
			capped++
		}
	}

	fmt.Fprintf(w, "suite %s — %d runs (%d replicates × %d tasks × %d variants)",
		r.Suite, len(r.Runs), r.Replicates, len(r.Runs)/max(1, r.Replicates*len(r.Variants)), len(r.Variants))
	if failed > 0 {
		fmt.Fprintf(w, ", %d failed", failed)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w)

	deltas := Deltas(r)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	header := "metric"
	for _, v := range r.Variants {
		header += "\t" + v
	}
	fmt.Fprintln(tw, header+"\tdelta")
	for _, metric := range order {
		row := metric
		for _, v := range r.Variants {
			s, ok := byMetric[metric][v]
			if !ok {
				row += "\t-"
				continue
			}
			row += fmt.Sprintf("\t%s ±%s (n=%d)", format(metric, s.Mean), format(metric, s.Stderr), s.N)
		}
		row += "\t" + deltaCell(metric, deltas)
		fmt.Fprintln(tw, row)
	}
	tw.Flush()

	fmt.Fprintln(w)
	fmt.Fprintln(w, "± is the standard error of the mean. A delta smaller than the two")
	fmt.Fprintln(w, "standard errors combined is not a result; add replicates.")
	if capped > 0 {
		fmt.Fprintf(w, "\n%d of %d runs hit the sub-turn cap. Their metrics stop where the run\n", capped, len(r.Runs))
		fmt.Fprintln(w, "stopped, not where the work did. Raise -max-sub-turns and run it again")
		fmt.Fprintln(w, "rather than reading these numbers.")
	}
}

// A Delta is one metric's comparison of the last variant against the first,
// which is the comparison a two-arm run is asking for. Significant is decided
// here rather than by each consumer, so the HTTP surface and the browser
// cannot disagree about when a difference is worth looking at.
type Delta struct {
	Metric          string  `json:"metric"`
	BaselineVariant string  `json:"baseline_variant"`
	Variant         string  `json:"variant"`
	Diff            float64 `json:"diff"`
	CombinedStderr  float64 `json:"combined_stderr"`
	Significant     bool    `json:"significant"`
}

// Deltas compares the last variant to the first, one entry per metric, in
// report order.
func Deltas(r *Report) []Delta {
	if len(r.Variants) < 2 {
		return nil
	}
	baseline, variant := r.Variants[0], r.Variants[len(r.Variants)-1]
	byMetric := map[string]map[string]Summary{}
	var order []string
	for _, s := range Summarise(r) {
		if _, ok := byMetric[s.Metric]; !ok {
			byMetric[s.Metric] = map[string]Summary{}
			order = append(order, s.Metric)
		}
		byMetric[s.Metric][s.Variant] = s
	}

	var out []Delta
	for _, metric := range order {
		base, hasBase := byMetric[metric][baseline]
		last, hasLast := byMetric[metric][variant]
		if !hasBase || !hasLast {
			continue
		}
		diff := last.Mean - base.Mean
		combined := math.Hypot(base.Stderr, last.Stderr)
		out = append(out, Delta{
			Metric: metric, BaselineVariant: baseline, Variant: variant,
			Diff: diff, CombinedStderr: combined,
			Significant: combined > 0 && math.Abs(diff) > 2*combined,
		})
	}
	return out
}

// deltaCell renders one metric's delta for the report table.
func deltaCell(metric string, deltas []Delta) string {
	for _, d := range deltas {
		if d.Metric != metric {
			continue
		}
		marker := ""
		if d.Significant {
			marker = " *"
		}
		return fmt.Sprintf("%+s%s", format(metric, d.Diff), marker)
	}
	return ""
}

// format prints a metric the way it reads: shares as percentages, money to
// four places, counts plainly.
func format(metric string, v float64) string {
	switch {
	case strings.HasSuffix(metric, "_rate"), strings.HasSuffix(metric, "_via_tool"),
		strings.HasSuffix(metric, "_decay"), metric == "judge_completed":
		return fmt.Sprintf("%.1f%%", v*100)
	case metric == "context_tokens_max":
		return fmt.Sprintf("%.0fk", v/1000)
	case metric == "cost_usd":
		return fmt.Sprintf("$%.4f", v)
	default:
		return fmt.Sprintf("%.2f", v)
	}
}
