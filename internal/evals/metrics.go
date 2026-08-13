// Package evals measures what a prompt change does to a session's behaviour.
// A suite of tasks is published through the WORK stream under two or more
// prompt variants, and every run is scored from its own stored events: the
// same path a production run takes, so a result means the real thing works
// (docs/EVALS.md).
package evals

import (
	"encoding/json"
	"regexp"
	"sort"
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
// whether a call happened, and whether it worked. contextTokens is how large
// the request was at the moment of the call, which is what the decay metrics
// below are measured against.
type toolCall struct {
	name          string
	args          map[string]any
	result        string
	isError       bool
	contextTokens int
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
	{
		name:        "search_via_tool_decay",
		description: "change in search-tool share from the run's first half to its second, by context size",
		compute:     decayOf(searchViaToolShare),
	},
	{
		name:        "edit_miss_rate_decay",
		description: "change in the share of edits refused for want of a read, first half to second",
		compute:     decayOf(editMissRate),
	},
	{
		name:        "context_tokens_max",
		description: "largest request the run made, as a check that it ran long enough for decay to mean anything",
		compute: func(calls []toolCall) (float64, bool) {
			maxTokens := 0
			for _, c := range calls {
				if c.contextTokens > maxTokens {
					maxTokens = c.contextTokens
				}
			}
			if maxTokens == 0 {
				return 0, false
			}
			return float64(maxTokens), true
		},
	},
}

// A rule the model is asked to follow decays as the context grows: measured
// over 85 production sessions, search-tool share fell from 41.7% under 16k to
// 3.5% above 128k, and edits refused for want of a read rose from 0% to 1.95%
// (docs/EVALS.md). A wording change that holds early and not late is worth
// telling apart from one that holds throughout, so every rule metric has a
// decay counterpart.
//
// The split is at the run's own median context size rather than a fixed token
// figure, so a short run and a long one both yield a number and neither is
// scored against a threshold it never reached. Read it beside
// context_tokens_max: a decay of zero on a run that never passed 20k says
// nothing about what happens at 200k.

// share is a metric computed over some subset of a run's calls.
type share func(calls []toolCall) (float64, bool)

func searchViaToolShare(calls []toolCall) (float64, bool) {
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
	if viaTool+viaBash == 0 {
		return 0, false
	}
	return float64(viaTool) / float64(viaTool+viaBash), true
}

func editMissRate(calls []toolCall) (float64, bool) {
	edits, misses := 0, 0
	for _, c := range calls {
		if c.name != "Edit" && c.name != "Write" {
			continue
		}
		edits++
		if strings.Contains(c.result, "has not been read in this session") {
			misses++
		}
	}
	if edits == 0 {
		return 0, false
	}
	return float64(misses) / float64(edits), true
}

// decayOf turns a share into the difference between the run's second half and
// its first. A run where either half has nothing to measure reports nothing:
// a decay against an absent baseline is not a number.
func decayOf(s share) func([]toolCall) (float64, bool) {
	return func(calls []toolCall) (float64, bool) {
		first, second := splitByContext(calls)
		early, okEarly := s(first)
		late, okLate := s(second)
		if !okEarly || !okLate {
			return 0, false
		}
		return late - early, true
	}
}

// splitByContext divides a run at its median context size, so each half holds
// the calls made while the request was small and large respectively. Splitting
// on context rather than on call index is what makes the number about the
// window: a run can make thirty calls inside one sub-turn without the context
// moving at all.
func splitByContext(calls []toolCall) (first, second []toolCall) {
	sized := make([]toolCall, 0, len(calls))
	for _, c := range calls {
		if c.contextTokens > 0 {
			sized = append(sized, c)
		}
	}
	if len(sized) < 2 {
		return nil, nil
	}
	tokens := make([]int, len(sized))
	for i, c := range sized {
		tokens[i] = c.contextTokens
	}
	sort.Ints(tokens)
	median := tokens[len(tokens)/2]
	for _, c := range sized {
		if c.contextTokens < median {
			first = append(first, c)
		} else {
			second = append(second, c)
		}
	}
	return first, second
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

// pairCalls joins each tool_call to its tool_result and stamps it with the
// context size of the sub-turn it belongs to. A call with no result — the run
// was cut off mid-flight — keeps an empty result rather than being dropped,
// because it still happened.
func pairCalls(events []store.Event) []toolCall {
	results := map[string]store.ToolResultPayload{}
	// A sub-turn that was retried commits a second usage event; the first one
	// is the context the sub-turn's calls were made against.
	contextBySubTurn := map[int]int{}
	for _, e := range events {
		switch e.Kind {
		case store.KindToolResult:
			var p store.ToolResultPayload
			if err := json.Unmarshal(e.Payload, &p); err == nil {
				results[p.ToolCallID] = p
			}
		case store.KindUsage:
			var p store.UsagePayload
			if err := json.Unmarshal(e.Payload, &p); err == nil {
				if _, seen := contextBySubTurn[p.SubTurn]; !seen {
					contextBySubTurn[p.SubTurn] = p.PromptTokens
				}
			}
		}
	}

	var calls []toolCall
	subTurn := 0
	for _, e := range events {
		if e.Kind == store.KindTurnStarted {
			var p struct {
				SubTurn int `json:"sub_turn"`
			}
			if json.Unmarshal(e.Payload, &p) == nil {
				subTurn = p.SubTurn
			}
			continue
		}
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
			name:          p.Name,
			args:          args,
			result:        res.Content,
			isError:       res.IsError,
			contextTokens: contextBySubTurn[subTurn],
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
