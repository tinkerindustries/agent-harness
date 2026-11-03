package session

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/guard"
	"github.com/mrgeoffrich/agent-harness/internal/pricing"
	"github.com/mrgeoffrich/agent-harness/internal/store"
	"github.com/mrgeoffrich/agent-harness/internal/tools"
)

// GuardCall is the bash call the autonomy guard is asked about.
type GuardCall struct {
	ID      string
	Command string
	Workdir string
	// Round is every tool call of the round the call belongs to, itself
	// included. Their results commit together once all have finished, so a
	// write in the round has a tool_call and no result when the guard runs.
	Round []store.ToolCallPayload
}

// GuardHistory is the part of a session's log the guard state is built from.
type GuardHistory struct {
	// Events are in the order they happened, oldest session first.
	Events []store.Event
	// Start is when the session began.
	Start time.Time
}

// maxGuardAncestors bounds the walk up a chain of compactions.
const maxGuardAncestors = 64

// LoadGuardHistory reads sess's event log and, when sess was forked by
// compaction, the logs of the sessions it continues. The successor's own log
// holds only its opening message, so the earlier writes and commands are
// found by following ParentID to the session the summary came from.
func LoadGuardHistory(ctx context.Context, st *store.Store, sess store.Session) (GuardHistory, error) {
	var chain [][]store.Event
	start := sess.CreatedAt
	cur := sess
	for i := 0; ; i++ {
		events, err := st.GetEvents(ctx, cur.ID)
		if err != nil {
			return GuardHistory{}, fmt.Errorf("session: guard history for %s: %w", cur.ID, err)
		}
		chain = append(chain, events)
		start = cur.CreatedAt
		if cur.ParentID == "" || priorCheckpoint(cur.SystemPrompt) == "" || i >= maxGuardAncestors {
			break
		}
		parent, err := st.GetSession(ctx, cur.ParentID)
		if err != nil {
			return GuardHistory{}, fmt.Errorf("session: guard history parent of %s: %w", cur.ID, err)
		}
		cur = parent
	}
	var all []store.Event
	for i := len(chain) - 1; i >= 0; i-- {
		all = append(all, chain[i]...)
	}
	return GuardHistory{Events: all, Start: start}, nil
}

// BuildGuardState builds the state the guard checks call with: history is the
// session's log as it stands, and the session's workspace is its harness.cwd.
func BuildGuardState(ctx context.Context, b guard.StateBuilder, workspace string, h GuardHistory, call GuardCall) guard.State {
	return b.Build(ctx, GuardInputs(workspace, h, call))
}

type guardBashArgs struct {
	Command string `json:"command"`
	Workdir string `json:"workdir"`
}

type guardOutcome struct {
	settled bool
	isError bool
	content string
	denied  bool
}

// GuardInputs reads the log for the facts the state needs that are not on
// disk: the earlier bash commands and the writes that succeeded.
func GuardInputs(workspace string, h GuardHistory, call GuardCall) guard.Inputs {
	in := guard.Inputs{
		Command:      call.Command,
		Workdir:      call.Workdir,
		Directory:    workspace,
		Workspace:    workspace,
		SessionStart: h.Start,
	}

	outcomes := map[string]guardOutcome{}
	for _, e := range h.Events {
		switch e.Kind {
		case store.KindToolResult:
			var p store.ToolResultPayload
			if json.Unmarshal(e.Payload, &p) == nil {
				outcomes[p.ToolCallID] = guardOutcome{settled: true, isError: p.IsError, content: p.Content}
			}
		case store.KindToolDenied:
			var p store.ToolDeniedPayload
			if json.Unmarshal(e.Payload, &p) == nil {
				outcomes[p.ToolCallID] = guardOutcome{settled: true, denied: true}
			}
		}
	}

	for _, e := range h.Events {
		if e.Kind != store.KindToolCall {
			continue
		}
		var p store.ToolCallPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			continue
		}
		if p.ID == call.ID {
			break
		}
		out := outcomes[p.ID]
		switch p.Name {
		case "bash":
			if out.denied {
				continue
			}
			var args guardBashArgs
			if json.Unmarshal([]byte(p.Arguments), &args) == nil && args.Command != "" {
				in.Commands = append(in.Commands, args.Command)
			}
		case "write", "edit", "apply_patch":
			if !out.settled || out.isError || out.denied {
				continue
			}
			created := map[string]bool{}
			for _, path := range tools.CreatedTargets(workspace, p.Name, p.Arguments, out.content) {
				created[path] = true
			}
			for _, path := range tools.MutationTargets(workspace, p.Name, p.Arguments) {
				in.Mutations = append(in.Mutations, guard.Mutation{Path: path, Created: created[path]})
			}
		}
	}

	for _, c := range call.Round {
		if c.ID == call.ID {
			continue
		}
		if _, done := outcomes[c.ID]; done {
			continue
		}
		in.Pending = append(in.Pending, tools.MutationTargets(workspace, c.Name, c.Arguments)...)
	}
	return in
}

// GuardEvents turns a decision into the events that record it: the decision
// itself and, when stage 2 answered, a usage event with the model set so the
// spend is separable from the session's own.
func GuardEvents(callID, mode string, d guard.Decision, prices *pricing.Table, sentAt time.Time) []store.EventInput {
	payload := store.GuardDecisionPayload{
		ToolCallID:      callID,
		Mode:            mode,
		GuardVersion:    d.GuardVersion,
		Verdict:         d.Verdict,
		DecidedBy:       d.DecidedBy,
		Rules:           d.Rules,
		Escalation:      d.Escalation,
		PBlock:          d.PBlock,
		Stage1Error:     d.Stage1Error,
		Stage2Ran:       d.Stage2Ran,
		Stage2Model:     d.Stage2Model,
		Stage2Reason:    d.Stage2Reason,
		Stage2Error:     d.Stage2Error,
		Stage1LatencyMs: d.Stage1Latency.Milliseconds(),
		Stage2LatencyMs: d.Stage2Latency.Milliseconds(),
		LatencyMs:       d.Latency.Milliseconds(),
	}
	inputs := []store.EventInput{}
	if u := d.Stage2Usage; u != nil {
		payload.Stage2Usage = &store.GuardUsage{
			InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
			CacheCreationInputTokens: u.CacheCreationInputTokens, CacheReadInputTokens: u.CacheReadInputTokens,
		}
		inputs = append(inputs, store.EventInput{Kind: store.KindUsage, Payload: guardUsagePayload(d.Stage2Model, *u, prices, sentAt)})
	}
	return append([]store.EventInput{{Kind: store.KindGuardDecision, Payload: payload}}, inputs...)
}

func guardUsagePayload(model string, u guard.Usage, prices *pricing.Table, sentAt time.Time) store.UsagePayload {
	p := store.UsagePayload{
		Model:                  model,
		PromptTokens:           u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens,
		PromptCacheHitTokens:   u.CacheReadInputTokens,
		PromptCacheMissTokens:  u.InputTokens,
		PromptCacheWriteTokens: u.CacheCreationInputTokens,
		CompletionTokens:       u.OutputTokens,
	}
	if prices != nil && strings.TrimSpace(model) != "" {
		if c, tier, err := prices.Cost(model, sentAt, p.PromptCacheHitTokens, p.PromptCacheMissTokens, p.PromptCacheWriteTokens, p.CompletionTokens); err == nil {
			p.CostUSD, p.RateTier = c, string(tier)
		}
	}
	return p
}
