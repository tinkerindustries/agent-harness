// Package session implements the agent loop: sub-turn iteration against
// DeepSeek, tool execution, and the session-runner interface that holds no
// state outside the session it is running (docs/DESIGN.md §4.5).
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/mrgeoffrich/deepseek-harness/internal/cache"
	"github.com/mrgeoffrich/deepseek-harness/internal/deepseek"
	"github.com/mrgeoffrich/deepseek-harness/internal/pricing"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
	"github.com/mrgeoffrich/deepseek-harness/internal/tools"
)

// DefaultMaxSubTurns bounds a run when RunOptions.MaxSubTurns is unset.
const DefaultMaxSubTurns = 100

// CompactionThresholdTokens is DeepSeek's recommended Claude Code
// compaction window, 768K of the 1M context (docs/TOOLS.md, "Context and
// compaction").
const CompactionThresholdTokens = 768 * 1024

// RunOptions is everything one session run needs. It is deliberately flat
// rather than reaching into global config, so a Runner can be shared by
// many concurrent calls to Run without any of them touching another's
// state.
type RunOptions struct {
	Model          string
	Effort         string
	Thinking       bool
	MaxTokens      int
	Workspace      string
	PermissionMode tools.Mode
	Deny           []string
	Prompt         string
	ResultSchema   json.RawMessage
	MaxSubTurns    int
	Resolver       tools.Resolver
	ParentID       string

	// Progress, when set, overrides Runner.Progress for this call only.
	// Concurrent callers that need to tell their sub-turn reports apart —
	// several CLI jobs sharing one Runner, say — set this instead of
	// relying on the single Runner-level hook (docs/DESIGN.md §4.5: a
	// session holds no state outside itself, and that includes which
	// closure reports its progress).
	Progress func(SubTurnProgress)
}

// Usage aggregates token accounting across every sub-turn of a run.
type Usage struct {
	CacheHitTokens   int
	CacheMissTokens  int
	CompletionTokens int
	ReasoningTokens  int
	CostUSD          float64
}

func (u *Usage) add(p store.UsagePayload) {
	u.CacheHitTokens += p.PromptCacheHitTokens
	u.CacheMissTokens += p.PromptCacheMissTokens
	u.CompletionTokens += p.CompletionTokens
	u.ReasoningTokens += p.ReasoningTokens
	u.CostUSD += p.CostUSD
}

// RunResult is what Run returns once a session reaches a terminal state.
type RunResult struct {
	SessionID string
	Status    string
	Text      string
	Result    json.RawMessage
	Summary   string
	SubTurns  int
	Usage     Usage
}

// SubTurnProgress is reported to Runner.Progress, when set, once per
// completed sub-turn — the seam a CLI or future SSE hub hangs off.
type SubTurnProgress struct {
	SessionID string
	SubTurn   int
	Model     string
	ToolCalls []string
	Usage     store.UsagePayload
	Churned   bool
}

// Runner executes agent sessions. Its fields are shared, read-mostly
// resources (a store with its own internal synchronisation, a stateless API
// client, a price table); nothing about one call to Run leaks into another.
// That is what lets phase 3 add a worker pool around this type without
// changing it (docs/DESIGN.md §4.5).
type Runner struct {
	Store         *store.Store
	Mirror        *store.Mirror
	Client        *deepseek.Client
	Prices        *pricing.Table
	FlashModel    string
	BashAllowlist []string

	MaxSubTurns               int
	CompactionThresholdTokens int

	// Progress, when set, is called after every sub-turn commits. It may be
	// called concurrently by different Run calls and must not block.
	Progress func(SubTurnProgress)
}

func (r *Runner) maxSubTurns() int {
	if r.MaxSubTurns > 0 {
		return r.MaxSubTurns
	}
	return DefaultMaxSubTurns
}

func (r *Runner) flashModel() string {
	if r.FlashModel != "" {
		return r.FlashModel
	}
	return "deepseek-v4-flash"
}

func (r *Runner) bashAllowlist() []string {
	if r.BashAllowlist != nil {
		return r.BashAllowlist
	}
	return tools.DefaultBashAllowlist
}

// progressFunc picks a call's progress hook: its own override if it set
// one, otherwise the Runner-level default.
func progressFunc(r *Runner, opts RunOptions) func(SubTurnProgress) {
	if opts.Progress != nil {
		return opts.Progress
	}
	return r.Progress
}

func (r *Runner) compactionThreshold() int {
	if r.CompactionThresholdTokens != 0 {
		return r.CompactionThresholdTokens
	}
	return CompactionThresholdTokens
}

// Run drives one session from creation to a terminal state: a response with
// no tool calls, a successful Complete call, exhausting MaxSubTurns, or an
// unrecoverable error. The returned error is non-nil only for
// infrastructure failures (store or stream errors); a task the model gave
// up on is a normal RunResult, not an error.
func (r *Runner) Run(ctx context.Context, opts RunOptions) (*RunResult, error) {
	if opts.Model == "" {
		return nil, errors.New("session: model is required")
	}
	if opts.Workspace == "" {
		return nil, errors.New("session: workspace is required")
	}
	if opts.PermissionMode == "" {
		opts.PermissionMode = tools.ModeDefault
	}

	policy := &tools.Policy{
		Mode:          opts.PermissionMode,
		Deny:          opts.Deny,
		BashAllowlist: r.bashAllowlist(),
		Resolver:      opts.Resolver,
	}
	executor, err := tools.NewExecutor(opts.Workspace, policy)
	if err != nil {
		return nil, err
	}
	executor.Client = r.Client
	executor.Prices = r.Prices
	executor.FlashModel = r.flashModel()
	executor.ResultSchema = opts.ResultSchema

	toolSchema, err := json.Marshal(tools.Definitions())
	if err != nil {
		return nil, fmt.Errorf("session: encode tool schema: %w", err)
	}

	sessID := newID("sess")
	executor.RunSubagent = r.subagentRunner(sessID, opts, executor.Workspace)

	sess := store.Session{
		ID:             sessID,
		ParentID:       opts.ParentID,
		Model:          opts.Model,
		Effort:         opts.Effort,
		Thinking:       opts.Thinking,
		Workspace:      executor.Workspace,
		PermissionMode: string(opts.PermissionMode),
		DenyPatterns:   opts.Deny,
		SystemPrompt:   RenderSystemPrompt(),
		ToolSchema:     toolSchema,
		ResultSchema:   opts.ResultSchema,
		Status:         store.StatusRunning,
	}
	if err := r.Store.CreateSession(ctx, sess); err != nil {
		return nil, fmt.Errorf("session: create session: %w", err)
	}
	curSess, err := r.Store.GetSession(ctx, sessID)
	if err != nil {
		return nil, fmt.Errorf("session: reload created session: %w", err)
	}
	if r.Mirror != nil {
		if err := r.Mirror.Init(curSess, nil); err != nil {
			log.Printf("session: mirror init failed for %s: %v", sessID, err)
		}
	}

	opening := RenderOpeningMessage(executor.Workspace, opts.Prompt, opts.ResultSchema)
	appended, err := r.Store.AppendEvents(ctx, sessID, []store.EventInput{
		{Kind: store.KindSessionStarted, Payload: store.SessionStartedPayload{OpeningMessage: opening}},
	})
	if err != nil {
		return r.fail(ctx, curSess, nil, 0, Usage{}, fmt.Errorf("session: record session start: %w", err))
	}
	r.mirrorAppend(curSess, appended)
	allEvents := appended

	detector := cache.NewDetector()
	var agg Usage
	var lastText string
	maxTurns := opts.MaxSubTurns
	if maxTurns <= 0 {
		maxTurns = r.maxSubTurns()
	}

	for subTurn := 1; subTurn <= maxTurns; subTurn++ {
		outcome, err := r.runSubTurn(ctx, curSess, &allEvents, opts, executor, detector, subTurn)
		if err != nil {
			return r.fail(ctx, curSess, allEvents, subTurn-1, agg, err)
		}
		agg.add(outcome.usagePayload)
		lastText = outcome.text

		if outcome.completed {
			return r.finishRun(ctx, curSess, allEvents, "complete", store.StatusOK,
				outcome.text, outcome.payload.Result, outcome.payload.Summary, outcome.payload.Status, agg, subTurn)
		}
		if !outcome.hasToolCalls {
			return r.finishRun(ctx, curSess, allEvents, "no_tool_calls", store.StatusOK,
				outcome.text, nil, "", "", agg, subTurn)
		}

		if outcome.usagePayload.PromptTokens >= r.compactionThreshold() {
			newSess, newEvents, err := r.compact(ctx, curSess, allEvents, executor.Workspace)
			if err != nil {
				log.Printf("session: compaction failed for %s, continuing uncompacted: %v", curSess.ID, err)
			} else {
				curSess = newSess
				allEvents = newEvents
			}
		}
	}

	return r.finishRun(ctx, curSess, allEvents, "max_sub_turns", store.StatusMaxTurns, lastText, nil, "", "", agg, maxTurns)
}

// subagentRunner builds the closure Task uses to delegate to a nested,
// flash-backed session. Its transcript never joins the parent's message
// array; only the text this returns does (docs/TOOLS.md).
func (r *Runner) subagentRunner(parentID string, parentOpts RunOptions, workspace string) func(context.Context, string, string, string) (string, error) {
	return func(ctx context.Context, description, prompt, subagentType string) (string, error) {
		res, err := r.Run(ctx, RunOptions{
			Model:          r.flashModel(),
			Effort:         deepseek.EffortHigh,
			Thinking:       true,
			MaxTokens:      20000,
			Workspace:      workspace,
			PermissionMode: parentOpts.PermissionMode,
			Deny:           parentOpts.Deny,
			Prompt:         prompt,
			Resolver:       parentOpts.Resolver,
			ParentID:       parentID,
		})
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(res.Text) != "" {
			return res.Text, nil
		}
		return res.Summary, nil
	}
}

func (r *Runner) mirrorAppend(sess store.Session, events []store.Event) {
	if r.Mirror == nil || len(events) == 0 {
		return
	}
	if err := r.Mirror.AppendEvents(sess, events); err != nil {
		log.Printf("session: mirror append failed for %s: %v", sess.ID, err)
	}
}

func (r *Runner) mirrorTranscript(sess store.Session, events []store.Event) {
	if r.Mirror == nil {
		return
	}
	if err := r.Mirror.WriteTranscript(sess, events); err != nil {
		log.Printf("session: mirror transcript failed for %s: %v", sess.ID, err)
	}
}

func (r *Runner) mirrorUpdateSession(sess store.Session) {
	if r.Mirror == nil {
		return
	}
	if err := r.Mirror.UpdateSession(sess); err != nil {
		log.Printf("session: mirror session update failed for %s: %v", sess.ID, err)
	}
}

// finishRun records run_finished, updates the session's terminal status,
// and rewrites the mirror one last time.
func (r *Runner) finishRun(ctx context.Context, sess store.Session, allEvents []store.Event,
	reason, sessionStatus, text string, result json.RawMessage, summary, completeStatus string,
	agg Usage, subTurns int) (*RunResult, error) {

	payload := store.RunFinishedPayload{Reason: reason, Text: text, Result: result, Summary: summary, Status: completeStatus}
	appended, err := r.Store.AppendEvents(ctx, sess.ID, []store.EventInput{{Kind: store.KindRunFinished, Payload: payload}})
	if err != nil {
		return &RunResult{SessionID: sess.ID, Status: store.StatusFailed, SubTurns: subTurns, Usage: agg}, err
	}
	r.mirrorAppend(sess, appended)
	allEvents = append(allEvents, appended...)

	finished := time.Now().UTC()
	if err := r.Store.UpdateSessionStatus(ctx, sess.ID, sessionStatus, &finished); err != nil {
		return &RunResult{SessionID: sess.ID, Status: sessionStatus, SubTurns: subTurns, Usage: agg}, err
	}
	if updated, err := r.Store.GetSession(ctx, sess.ID); err == nil {
		r.mirrorUpdateSession(updated)
		r.mirrorTranscript(updated, allEvents)
	}

	return &RunResult{
		SessionID: sess.ID, Status: sessionStatus, Text: text, Result: result, Summary: summary,
		SubTurns: subTurns, Usage: agg,
	}, nil
}

// fail records an error event and marks the session failed. It always
// returns a non-nil error alongside a best-effort RunResult.
func (r *Runner) fail(ctx context.Context, sess store.Session, allEvents []store.Event, subTurns int, agg Usage, cause error) (*RunResult, error) {
	appended, appendErr := r.Store.AppendEvents(ctx, sess.ID, []store.EventInput{
		{Kind: store.KindError, Payload: store.ErrorPayload{Message: cause.Error()}},
	})
	if appendErr == nil {
		r.mirrorAppend(sess, appended)
		allEvents = append(allEvents, appended...)
	}

	finished := time.Now().UTC()
	_ = r.Store.UpdateSessionStatus(ctx, sess.ID, store.StatusFailed, &finished)
	if updated, err := r.Store.GetSession(ctx, sess.ID); err == nil {
		r.mirrorUpdateSession(updated)
		r.mirrorTranscript(updated, allEvents)
	}

	return &RunResult{SessionID: sess.ID, Status: store.StatusFailed, SubTurns: subTurns, Usage: agg}, cause
}

// executeToolCalls runs every call concurrently and returns outcomes in the
// same order as calls, regardless of completion order — the appender is
// what must preserve tool_calls order, not the execution itself
// (docs/TOOLS.md).
func executeToolCalls(ctx context.Context, executor *tools.Executor, calls []deepseek.AssembledToolCall) []tools.Outcome {
	outcomes := make([]tools.Outcome, len(calls))
	var wg sync.WaitGroup
	for i, c := range calls {
		wg.Add(1)
		go func(i int, c deepseek.AssembledToolCall) {
			defer wg.Done()
			call := deepseek.ToolCall{
				ID:   c.ID,
				Type: "function",
				Function: deepseek.ToolCallFunc{
					Name:      c.Name,
					Arguments: c.Arguments,
				},
			}
			outcomes[i] = executor.Execute(ctx, call)
		}(i, c)
	}
	wg.Wait()
	return outcomes
}

func toolCallNames(calls []deepseek.AssembledToolCall) []string {
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = c.Name
	}
	return out
}
