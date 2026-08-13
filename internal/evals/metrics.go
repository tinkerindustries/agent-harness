// Package evals measures what a prompt change does to a session's behaviour.
// A suite of tasks is published through the WORK stream under two or more
// prompt variants, and every run is scored from its own stored events: the
// same path a production run takes, so a result means the real thing works
// (docs/EVALS.md).
package evals

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

// Scores are one run's mechanical metrics, keyed by metric name. Every value
// is derived from stored events, so re-scoring a session gives the same
// numbers however long after the run it happens.
type Scores map[string]float64

// A metric reduces one session's tool calls to a single number, or reports
// that the session gave it nothing to measure — a run that never searched has
// no search-tool share, and averaging a zero in would drag the mean toward a
// behaviour that did not happen.
type metric struct {
	name        string
	description string
	compute     func(calls []toolCall) (float64, bool)
}

// toolCall pairs a call with its result, which is what most metrics need:
// whether a call happened, and whether it worked.
type toolCall struct {
	name    string
	args    map[string]any
	result  string
	isError bool
}

func (c toolCall) arg(key string) string {
	if v, ok := c.args[key].(string); ok {
		return v
	}
	return ""
}

// bashSearch matches a shell command that is looking for a file or a string
// rather than doing something to one. Anchored on a command boundary so a
// path like src/grep/main.go does not count.
var bashSearch = regexp.MustCompile(`(^|[|;&(]|\s)(grep|rg|ag|ack|find|fd|ls -R)\b`)

// Metrics is every mechanical metric, in report order.
var Metrics = []metric{
	{
		name:        "search_via_tool",
		description: "share of searches made with Grep or Glob rather than through Bash",
		compute: func(calls []toolCall) (float64, bool) {
			viaTool, viaBash := 0, 0
			for _, c := range calls {
				switch c.name {
				case "Grep", "Glob":
					viaTool++
				case "Bash":
					if bashSearch.MatchString(c.arg("command")) {
						viaBash++
					}
				}
			}
			total := viaTool + viaBash
			if total == 0 {
				return 0, false
			}
			return float64(viaTool) / float64(total), true
		},
	},
	{
		name:        "searches_total",
		description: "searches of any kind, as a check that a variant did not simply search less",
		compute: func(calls []toolCall) (float64, bool) {
			n := 0
			for _, c := range calls {
				if c.name == "Grep" || c.name == "Glob" || (c.name == "Bash" && bashSearch.MatchString(c.arg("command"))) {
					n++
				}
			}
			return float64(n), true
		},
	},
	{
		name:        "tool_error_rate",
		description: "share of all tool calls that came back an error",
		compute: func(calls []toolCall) (float64, bool) {
			if len(calls) == 0 {
				return 0, false
			}
			errs := 0
			for _, c := range calls {
				if c.isError {
					errs++
				}
			}
			return float64(errs) / float64(len(calls)), true
		},
	},
	{
		name:        "read_before_edit_misses",
		description: "Edit or Write calls refused because the file had not been read",
		compute: func(calls []toolCall) (float64, bool) {
			n := 0
			for _, c := range calls {
				if c.name != "Edit" && c.name != "Write" {
					continue
				}
				if strings.Contains(c.result, "has not been read in this session") {
					n++
				}
			}
			return float64(n), true
		},
	},
	{
		name:        "tool_calls",
		description: "tool calls in the run",
		compute: func(calls []toolCall) (float64, bool) {
			return float64(len(calls)), true
		},
	},
}

// Score computes every metric over one session's events. A metric with
// nothing to measure is absent from the result rather than zero.
func Score(events []store.Event) Scores {
	calls := pairCalls(events)
	scores := Scores{}
	for _, m := range Metrics {
		if v, ok := m.compute(calls); ok {
			scores[m.name] = v
		}
	}
	return scores
}

// pairCalls joins each tool_call to its tool_result. A call with no result —
// the run was cut off mid-flight — keeps an empty result rather than being
// dropped, because it still happened.
func pairCalls(events []store.Event) []toolCall {
	results := map[string]store.ToolResultPayload{}
	for _, e := range events {
		if e.Kind != store.KindToolResult {
			continue
		}
		var p store.ToolResultPayload
		if err := json.Unmarshal(e.Payload, &p); err == nil {
			results[p.ToolCallID] = p
		}
	}

	var calls []toolCall
	for _, e := range events {
		if e.Kind != store.KindToolCall {
			continue
		}
		var p store.ToolCallPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			continue
		}
		args := map[string]any{}
		_ = json.Unmarshal([]byte(p.Arguments), &args)
		res := results[p.ID]
		calls = append(calls, toolCall{
			name:    p.Name,
			args:    args,
			result:  res.Content,
			isError: res.IsError,
		})
	}
	return calls
}

// MetricDescription is the one-line summary printed beside a metric.
func MetricDescription(name string) string {
	for _, m := range Metrics {
		if m.name == name {
			return m.description
		}
	}
	return ""
}
