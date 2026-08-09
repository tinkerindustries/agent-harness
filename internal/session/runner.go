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
	"github.com/mrgeoffrich/deepseek-harness/internal/hub"
	"github.com/mrgeoffrich/deepseek-harness/internal/pricing"
	"github.com/mrgeoffrich/deepseek-harness/internal/skills"
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

	// SessionID, when set, is used instead of generating a fresh one. A
	// caller that must know the id before the session row exists — the
	// worker pool acquiring a workspace lease under it before calling Run,
	// say — generates one with NewSessionID and passes it back here so the
	// lease and the session agree.
	SessionID string

	// Progress, when set, overrides Runner.Progress for this call only.
	// Concurrent callers that need to tell their sub-turn reports apart —
	// several CLI jobs sharing one Runner, say — set this instead of
	// relying on the single Runner-level hook (docs/DESIGN.md §4.5: a
	// session holds no state outside itself, and that includes which
	// closure reports its progress).
	Progress func(SubTurnProgress)

	// DebugChurnAtSubTurn, when equal to a sub-turn number, deliberately
	// breaks that one sub-turn's shared prefix before sending it (via
	// cache.Mutate on the opening message) so the churn diagnostic has
	// something real to catch. Zero disables it. Nothing publishes this
	// through a work request; it exists to exercise CACHE.md's diagnostic
	// against the live API on purpose (PLAN.md phase 6's exit criterion),
	// not as something a production caller would ever set.
	DebugChurnAtSubTurn int
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

// RunResult is what Run (or Resume) returns once a session reaches a
// terminal state. SubTurns is the session's absolute sub-turn count at that
// point, so it reads the same way after a Resume as after a fresh Run; Usage
// is only this call's own contribution (the sub-turns it actually ran), not
// the session's lifetime total — store.SessionUsageSummaries has that.
type RunResult struct {
	SessionID string
	Status    string
	Text      string
	Result    json.RawMessage
	Summary   string
	SubTurns  int
	Usage     Usage
	// CompleteStatus is the status argument to Complete, when the model
	// called it: "done" or "gave_up". Empty means Complete was never
	// called, which is a normal outcome — thinking mode cannot force a
	// tool call (docs/DESIGN.md §4.6) — not a sign of anything wrong.
	CompleteStatus string
}

// SubTurnProgress is reported to Runner.Progress, when set, once per
// completed sub-turn. It is a summary at sub-turn granularity, so it suits a
// CLI line or the session-list feed. The transcript firehose publishes every
// event through Runner.Hub instead.
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
	Store      *store.Store
	Mirror     *store.Mirror
	Client     *deepseek.Client
	Prices     *pricing.Table
	FlashModel string

	// Hub, when set, is where every committed event and every session
	// state change gets published for a browser to watch live. Nil is the
	// CLI's normal case: nothing subscribes, so nothing is published.
	Hub *hub.Hub

	MaxSubTurns               int
	CompactionThresholdTokens int

	// Progress, when set, is called after every sub-turn commits. It may be
	// called concurrently by different Run calls and must not block.
	Progress func(SubTurnProgress)

	// ModelLimits caps concurrent in-flight DeepSeek requests per model,
	// shared across every call to Run on this Runner — the semaphore
	// docs/DESIGN.md §4.5 sizes under the account's per-model ceiling. A
	// model absent from the map, or a nil map, is unlimited; that keeps
	// existing callers (the CLI, every phase 2 test) exactly as they were.
	ModelLimits map[string]int

	semsMu sync.Mutex
	sems   map[string]chan struct{}
}

// acquireModelSlot blocks until a concurrent-request slot for model is
// free, or ctx is done. The returned release func is always safe to call
// once; a caller with no limit configured gets a no-op.
func (r *Runner) acquireModelSlot(ctx context.Context, model string) (func(), error) {
	limit := r.ModelLimits[model]
	if limit <= 0 {
		return func() {}, nil
	}
	r.semsMu.Lock()
	if r.sems == nil {
		r.sems = make(map[string]chan struct{})
	}
	sem, ok := r.sems[model]
	if !ok {
		sem = make(chan struct{}, limit)
		r.sems[model] = sem
	}
	r.semsMu.Unlock()

	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
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
	if !opts.PermissionMode.Valid() {
		return nil, fmt.Errorf("session: permission mode %q must be readonly or full", opts.PermissionMode)
	}

	policy := &tools.Policy{
		Mode:     opts.PermissionMode,
		Deny:     opts.Deny,
		Resolver: opts.Resolver,
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

	sessID := opts.SessionID
	if sessID == "" {
		sessID = newID("sess")
	}
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
	r.publishState(ctx, curSess)

	// Discovery runs per session, including for each Task subagent, because a
	// subagent works the same workspace and benefits from the same skills.
	catalogue := skills.Discover(executor.Workspace).Render()
	opening := RenderOpeningMessage(executor.Workspace, opts.Prompt, opts.ResultSchema, catalogue)
	appended, err := r.Store.AppendEvents(ctx, sessID, []store.EventInput{
		{Kind: store.KindSessionStarted, Payload: store.SessionStartedPayload{
			OpeningMessage: opening,
			SkillCatalogue: catalogue,
		}},
	})
	if err != nil {
		return r.fail(ctx, curSess, nil, 0, Usage{}, fmt.Errorf("session: record session start: %w", err))
	}
	r.mirrorAppend(curSess, appended)
	r.publishEvents(curSess, appended)
	allEvents := appended

	return r.runLoop(ctx, curSess, allEvents, opts, executor, cache.NewDetector(), 1)
}

// runLoop iterates sub-turns from startSubTurn through opts.MaxSubTurns (or
// the Runner default) until the session reaches a terminal state. Both Run
// and Resume end by calling this; they differ only in how curSess, allEvents,
// executor, and detector got built — a fresh session versus one reloaded
// from the store — and in where their own numbering starts.
func (r *Runner) runLoop(ctx context.Context, curSess store.Session, allEvents []store.Event, opts RunOptions,
	executor *tools.Executor, detector *cache.Detector, startSubTurn int) (*RunResult, error) {

	var agg Usage
	var lastText string
	maxTurns := opts.MaxSubTurns
	if maxTurns <= 0 {
		maxTurns = r.maxSubTurns()
	}

	for subTurn := startSubTurn; subTurn <= maxTurns; subTurn++ {
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
// array; only the text this returns does (docs/TOOLS.md). The session id it
// also returns is what lets the browser find and render that transcript as a
// collapsed child of the Task call that spawned it (PLAN.md phase 5).
func (r *Runner) subagentRunner(parentID string, parentOpts RunOptions, workspace string) func(context.Context, string, string, string) (string, string, error) {
	return func(ctx context.Context, description, prompt, subagentType string) (string, string, error) {
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
			if res != nil {
				return "", res.SessionID, err
			}
			return "", "", err
		}
		if strings.TrimSpace(res.Text) != "" {
			return res.Text, res.SessionID, nil
		}
		return res.Summary, res.SessionID, nil
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

// publishEvents fans newly committed events out to live SSE subscribers, on
// top of the disk mirror. Call it with the same events a successful
// AppendEvents just returned, alongside the mirrorAppend call for the same
// batch.
func (r *Runner) publishEvents(sess store.Session, events []store.Event) {
	if r.Hub == nil || len(events) == 0 {
		return
	}
	r.Hub.PublishEvents(sess.ID, events)
}

// publishState recomputes sess's session-list row and fans it out to the
// list stream (docs/DESIGN.md §5.8). It reads the usage summary and request
// id back from the store rather than threading them through the run loop,
// so the row a live subscriber sees is always exactly what a fresh GET
// /api/sessions would return for the same session.
func (r *Runner) publishState(ctx context.Context, sess store.Session) {
	if r.Hub == nil {
		return
	}
	summaries, err := r.Store.SessionUsageSummaries(ctx, []string{sess.ID})
	if err != nil {
		log.Printf("session: usage summary for %s: %v", sess.ID, err)
		return
	}
	requestIDs, err := r.Store.RequestIDsForSessions(ctx, []string{sess.ID})
	if err != nil {
		log.Printf("session: request id lookup for %s: %v", sess.ID, err)
		return
	}
	r.Hub.PublishSessionState(hub.BuildSessionState(sess, summaries[sess.ID], requestIDs[sess.ID], r.priceTableDate()))
}

// priceTableDate is r.Prices's capture date, or "" when this Runner has no
// price table (a test double, most often) — BuildSessionState treats an
// empty date as "not shown" rather than a zero value worth displaying.
func (r *Runner) priceTableDate() string {
	if r.Prices == nil {
		return ""
	}
	return r.Prices.CapturedAt
}

// finishRun records run_finished, updates the session's terminal status,
// and rewrites the mirror one last time.
func (r *Runner) finishRun(ctx context.Context, sess store.Session, allEvents []store.Event,
	reason, sessionStatus, text string, result json.RawMessage, summary, completeStatus string,
	agg Usage, subTurns int) (*RunResult, error) {

	payload := store.RunFinishedPayload{Reason: reason, Text: text, Result: result, Summary: summary, Status: completeStatus}
	appended, err := r.Store.AppendEvents(ctx, sess.ID, []store.EventInput{{Kind: store.KindRunFinished, Payload: payload}})
	if err != nil {
		return &RunResult{SessionID: sess.ID, Status: store.StatusFailed, SubTurns: subTurns, Usage: agg, CompleteStatus: completeStatus}, err
	}
	r.mirrorAppend(sess, appended)
	r.publishEvents(sess, appended)
	allEvents = append(allEvents, appended...)

	finished := time.Now().UTC()
	if err := r.Store.UpdateSessionStatus(ctx, sess.ID, sessionStatus, &finished); err != nil {
		return &RunResult{SessionID: sess.ID, Status: sessionStatus, SubTurns: subTurns, Usage: agg, CompleteStatus: completeStatus}, err
	}
	if updated, err := r.Store.GetSession(ctx, sess.ID); err == nil {
		r.mirrorUpdateSession(updated)
		r.mirrorTranscript(updated, allEvents)
		r.publishState(ctx, updated)
	}

	return &RunResult{
		SessionID: sess.ID, Status: sessionStatus, Text: text, Result: result, Summary: summary,
		SubTurns: subTurns, Usage: agg, CompleteStatus: completeStatus,
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
		r.publishEvents(sess, appended)
		allEvents = append(allEvents, appended...)
	}

	finished := time.Now().UTC()
	_ = r.Store.UpdateSessionStatus(ctx, sess.ID, store.StatusFailed, &finished)
	if updated, err := r.Store.GetSession(ctx, sess.ID); err == nil {
		r.mirrorUpdateSession(updated)
		r.mirrorTranscript(updated, allEvents)
		r.publishState(ctx, updated)
	}

	return &RunResult{SessionID: sess.ID, Status: store.StatusFailed, SubTurns: subTurns, Usage: agg}, cause
}

// executeToolCalls runs every call concurrently and returns outcomes in the
// same order as calls, regardless of completion order — the appender is
// what must preserve tool_calls order, not the execution itself
// (docs/TOOLS.md). A Bash call gets a live stdout sink wired through its
// context so the browser can show output as it happens instead of only on
// completion (docs/DESIGN.md §5.2); every other tool runs exactly as before.
func (r *Runner) executeToolCalls(ctx context.Context, sess store.Session, executor *tools.Executor, calls []deepseek.AssembledToolCall) []tools.Outcome {
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
			callCtx := ctx
			if r.Hub != nil && c.Name == "Bash" {
				callCtx = tools.WithStdoutSink(ctx, r.stdoutSink(ctx, sess, c.ID))
			}
			outcomes[i] = executor.Execute(callCtx, call)
		}(i, c)
	}
	wg.Wait()
	return outcomes
}

// stdoutSink returns the callback a running Bash call uses to publish
// incremental output. Each chunk commits as its own tool_stdout event,
// mirrored and fanned out to SSE subscribers exactly like any other event
// batch (docs/DESIGN.md §4.1); liveStdoutWriter is what keeps the volume of
// chunks down, not this function. It is never added to the fold's local
// event slice — internal/fold already treats tool_stdout as carrying no
// messages-array content, so there is nothing for the next request to gain
// from holding onto it after it is published.
func (r *Runner) stdoutSink(ctx context.Context, sess store.Session, toolCallID string) func(string) {
	return func(chunk string) {
		appended, err := r.Store.AppendEvents(ctx, sess.ID, []store.EventInput{
			{Kind: store.KindToolStdout, Payload: store.ToolStdoutPayload{ToolCallID: toolCallID, Text: chunk}},
		})
		if err != nil {
			log.Printf("session: append tool_stdout for %s: %v", sess.ID, err)
			return
		}
		r.mirrorAppend(sess, appended)
		r.publishEvents(sess, appended)
	}
}

func toolCallNames(calls []deepseek.AssembledToolCall) []string {
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = c.Name
	}
	return out
}
