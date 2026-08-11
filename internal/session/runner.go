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

	"github.com/mrgeoffrich/deepseek-harness/internal/agentmeta"
	"github.com/mrgeoffrich/deepseek-harness/internal/cache"
	"github.com/mrgeoffrich/deepseek-harness/internal/claudemd"
	"github.com/mrgeoffrich/deepseek-harness/internal/deepseek"
	"github.com/mrgeoffrich/deepseek-harness/internal/gemini"
	"github.com/mrgeoffrich/deepseek-harness/internal/httplog"
	"github.com/mrgeoffrich/deepseek-harness/internal/hub"
	"github.com/mrgeoffrich/deepseek-harness/internal/pricing"
	"github.com/mrgeoffrich/deepseek-harness/internal/settings"
	"github.com/mrgeoffrich/deepseek-harness/internal/skills"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
	"github.com/mrgeoffrich/deepseek-harness/internal/tools"
)

// DefaultMaxSubTurns and CompactionThresholdTokens are the built-in run
// budget defaults when the Runner has no settings resolver attached (the
// test path). Production resolves both through the settings registry
// (run.max_sub_turns, run.compaction_threshold), which carries these exact
// values as defaults; the two are pinned equal by
// internal/settings/registry_test.go. The sub-turn budget's other home —
// config's old defaultMaxSubTurns — was folded into the registry alongside
// this one, so both read the same setting and cannot drift again
// (docs/DESIGN.md §4.10).
const DefaultMaxSubTurns = 400

// CompactionThresholdTokens is DeepSeek's recommended Claude Code
// compaction window, 768K of the 1M context (docs/TOOLS.md, "Context and
// compaction").
const CompactionThresholdTokens = 768 * 1024

// RunOptions is everything one session run needs. It is deliberately flat
// rather than reaching into global config, so a Runner can be shared by
// many concurrent calls to Run without any of them touching another's
// state.
type RunOptions struct {
	Model           string
	Effort          string
	Thinking        bool
	MaxTokens       int
	Workspace       string
	PermissionMode  tools.Mode
	Deny            []string
	Prompt          string
	ResultSchema    json.RawMessage
	MaxSubTurns     int
	Resolver        tools.Resolver
	ParentID        string
	JobType         string
	ParentAgentType string
	ParentAgentID   string
	// ParentIsUser records that a person started this run directly. It is
	// producer-stamped and inherited by child sessions, like the rest of the
	// provenance triple.
	ParentIsUser bool

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
	// against the live API on purpose, not as something a production
	// caller would ever set.
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
	// Reason is run_finished's reason for the run ending: "complete",
	// "no_tool_calls", "max_sub_turns", "complete_rejected". Status alone
	// does not separate the ways a run can end without a valid result, and
	// the queue result's error code is derived from this.
	Reason string
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
// That is what lets the worker pool wrap this type without changing it
// (docs/DESIGN.md §4.5).
type Runner struct {
	Store      *store.Store
	Mirror     *store.Mirror
	Client     *deepseek.Client
	Prices     *pricing.Table
	FlashModel string

	// Gemini is the client the ReviewScreenshot tool uses to send screenshots
	// to Google's Gemini API. Nil is the CLI's case when none is configured:
	// the tool then reports itself unavailable instead of failing the run
	// (internal/tools, ReviewScreenshot).
	Gemini *gemini.Client

	// GeminiModel resolves the vision model name per call — the same
	// read-through-the-store shape as the DeepSeek API key provider, so a
	// model changed with `harness config set google.vision_model` takes
	// effect without a restart. Nil falls back to gemini.DefaultModel.
	GeminiModel func() (string, error)

	// Hub, when set, is where every committed event and every session
	// state change gets published for a browser to watch live. Nil is the
	// CLI's normal case: nothing subscribes, so nothing is published.
	Hub *hub.Hub

	// Recorder, when set, captures every HTTP exchange a session makes into
	// its own file under the recorder's root (docs/DESIGN.md §4.8). Nil is
	// the normal case when capture is off; every use is guarded. The runner
	// opens a session's writer where its row is created and closes it when
	// the run ends, so compacted successors and Task subagents each get
	// their own file.
	Recorder *httplog.Recorder

	MaxSubTurns               int
	CompactionThresholdTokens int

	// Settings, when set, is where the run budget (run.max_sub_turns,
	// run.compaction_threshold) and the flash model (model.flash) resolve
	// from, read through the store on every call, so a key changed with
	// `harness config set` takes effect on the next session without a
	// restart. Nil is the test path: the package constants apply.
	Settings *settings.Resolver

	// Progress, when set, is called after every sub-turn commits. It may be
	// called concurrently by different Run calls and must not block.
	Progress func(SubTurnProgress)

	// ModelLimits caps concurrent in-flight DeepSeek requests per model,
	// shared across every call to Run on this Runner — the semaphore
	// docs/DESIGN.md §4.5 sizes under the account's per-model ceiling. A
	// model absent from the map, or a nil map, is unlimited; that keeps
	// unlimited callers (the CLI, every test) exactly as they were.
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

func (r *Runner) maxSubTurns(ctx context.Context) int {
	if r.MaxSubTurns > 0 {
		return r.MaxSubTurns
	}
	if r.Settings != nil {
		if v, err := r.Settings.Int(ctx, settings.KeyRunMaxSubTurns); err == nil {
			return v
		}
	}
	return DefaultMaxSubTurns
}

func (r *Runner) flashModel(ctx context.Context) string {
	if r.FlashModel != "" {
		return r.FlashModel
	}
	if r.Settings != nil {
		if m, err := r.Settings.String(ctx, settings.KeyDefaultFlashModel); err == nil && m != "" {
			return m
		}
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

func (r *Runner) compactionThreshold(ctx context.Context) int {
	if r.CompactionThresholdTokens != 0 {
		return r.CompactionThresholdTokens
	}
	if r.Settings != nil {
		if v, err := r.Settings.Int(ctx, settings.KeyRunCompactionThreshold); err == nil {
			return v
		}
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
	executor.FlashModel = r.flashModel(ctx)
	executor.Gemini = r.Gemini
	executor.GeminiModel = r.GeminiModel
	executor.Settings = r.Settings
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
		ID:              sessID,
		ParentID:        opts.ParentID,
		JobType:         opts.JobType,
		ParentAgentType: opts.ParentAgentType,
		ParentAgentID:   opts.ParentAgentID,
		ParentIsUser:    opts.ParentIsUser,
		Model:           opts.Model,
		Effort:          opts.Effort,
		Thinking:        opts.Thinking,
		Workspace:       executor.Workspace,
		PermissionMode:  string(opts.PermissionMode),
		DenyPatterns:    opts.Deny,
		SystemPrompt:    RenderSystemPrompt(),
		ToolSchema:      toolSchema,
		ResultSchema:    opts.ResultSchema,
		Status:          store.StatusRunning,
	}
	if err := r.Store.CreateSession(ctx, sess); err != nil {
		return nil, fmt.Errorf("session: create session: %w", err)
	}
	curSess, err := r.Store.GetSession(ctx, sessID)
	if err != nil {
		return nil, fmt.Errorf("session: reload created session: %w", err)
	}
	r.openLog(curSess)
	if r.Mirror != nil {
		if err := r.Mirror.Init(curSess, nil); err != nil {
			log.Printf("session: mirror init failed for %s: %v", sessID, err)
		}
	}
	r.publishState(ctx, curSess)

	// Discovery runs per session, including for each Task subagent, because a
	// subagent works the same workspace and benefits from the same skills and
	// repository instructions.
	catalogue := skills.Discover(executor.Workspace).Render()
	claudeMD := claudemd.Discover(executor.Workspace).Render()
	opening := RenderOpeningMessage(executor.Workspace, opts.Prompt, opts.ResultSchema, claudeMD, catalogue)
	appended, err := r.Store.AppendEvents(ctx, sessID, []store.EventInput{
		{Kind: store.KindSessionStarted, Payload: store.SessionStartedPayload{
			OpeningMessage: opening,
			SkillCatalogue: catalogue,
			ClaudeMDBlock:  claudeMD,
			Task:           opts.Prompt,
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
		maxTurns = r.maxSubTurns(ctx)
	}

	// The applied-steer high-water mark is derived from the log, once, when a
	// run starts or resumes — never carried in memory across runs, so Resume
	// and a compacted successor both recompute it and a steer is never
	// delivered twice (docs/RUN-CONTROL.md "How the loop picks one up"). The
	// one rough edge of the two-kind design, worth saying here rather than
	// leaving a later reader to work out: a compacted session is a NEW session
	// id with a fresh log, so steers applied before compaction live in the
	// summary that carried them forward, and unapplied ones stay attached to
	// the retired session — they are neither delivered nor lost.
	appliedSeq, err := r.Store.LastAppliedSteerSeq(ctx, curSess.ID)
	if err != nil {
		return r.fail(ctx, curSess, allEvents, startSubTurn-1, agg, fmt.Errorf("session: derive applied steer seq: %w", err))
	}

	// A browser-started run is created with no prompt: the operator types
	// the first message into the session page's composer, and the page's
	// empty state promises that nothing has been sent to the model until
	// then (docs/RUN-CONTROL.md "The frontend"). So a fresh run with an
	// empty task waits for that first steer before the first request —
	// sending "Task:" with no task burns sub-turns asking what to do and
	// ends the run (no_tool_calls) before the operator can type, which the
	// live phase 5 run exposed. Only the empty-prompt start waits: every
	// other ingress (publish, MCP, subagents, resume) names a task. The
	// wait is bounded by the run's own budget — ctx carries the request
	// deadline and the stop cancellation — so an abandoned empty run
	// expires at its deadline and a stop ends it immediately.
	if startSubTurn == 1 && opts.Prompt == "" {
		if err := r.waitForFirstSteer(ctx, curSess.ID); err != nil {
			return r.fail(ctx, curSess, allEvents, startSubTurn-1, agg, err)
		}
	}

	// Consecutive Complete rejections carrying the same message: see
	// maxCompleteRejections.
	lastCompleteError, completeRejections := "", 0

	for subTurn := startSubTurn; subTurn <= maxTurns; subTurn++ {
		outcome, err := r.runSubTurn(ctx, curSess, &allEvents, opts, executor, detector, subTurn, &appliedSeq)
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

		switch {
		case outcome.completeError == "":
			// Anything other than a rejected Complete is progress by
			// definition: the model is doing something else.
			lastCompleteError, completeRejections = "", 0
		case outcome.completeError == lastCompleteError:
			completeRejections++
		default:
			lastCompleteError, completeRejections = outcome.completeError, 1
		}
		if completeRejections >= maxCompleteRejections {
			return r.abandonRun(ctx, curSess, allEvents, agg, subTurn, lastText, lastCompleteError)
		}

		if outcome.usagePayload.PromptTokens >= r.compactionThreshold(ctx) {
			newSess, newEvents, err := r.compact(ctx, curSess, allEvents, executor.Workspace)
			if err != nil {
				log.Printf("session: compaction failed for %s, continuing uncompacted: %v", curSess.ID, err)
			} else {
				curSess = newSess
				allEvents = newEvents
				// The compacted child is a new session with a fresh log, so
				// the applied-steer mark is re-derived for it rather than
				// carried over from the retired session (see the comment where
				// appliedSeq is first derived above).
				appliedSeq, err = r.Store.LastAppliedSteerSeq(ctx, curSess.ID)
				if err != nil {
					return r.fail(ctx, curSess, allEvents, subTurn-1, agg, fmt.Errorf("session: derive applied steer seq: %w", err))
				}
			}
		}
	}

	return r.finishRun(ctx, curSess, allEvents, "max_sub_turns", store.StatusMaxTurns, lastText, nil, "", "", agg, maxTurns)
}

// subagentRunner builds the closure Task uses to delegate to a nested,
// flash-backed session. Its transcript never joins the parent's message
// array; only the text this returns does (docs/TOOLS.md). The session id it
// also returns is what lets the browser find and render that transcript as a
// collapsed child of the Task call that spawned it.
func (r *Runner) subagentRunner(parentID string, parentOpts RunOptions, workspace string) func(context.Context, string, string, string) (string, string, error) {
	return func(ctx context.Context, description, prompt, subagentType string) (string, string, error) {
		// A subagent does work rather than orchestrating it, so its job type
		// is always implementation. It inherits the parent's owner, because
		// delegation does not change who owns the work and ParentID already
		// records the lineage — and that inheritance includes whether the
		// owner was a person.
		res, err := r.Run(ctx, RunOptions{
			Model:           r.flashModel(ctx),
			Effort:          deepseek.EffortMax,
			Thinking:        true,
			MaxTokens:       20000,
			Workspace:       workspace,
			PermissionMode:  parentOpts.PermissionMode,
			Deny:            parentOpts.Deny,
			Prompt:          prompt,
			Resolver:        parentOpts.Resolver,
			ParentID:        parentID,
			JobType:         agentmeta.JobTypeImplementation,
			ParentAgentType: parentOpts.ParentAgentType,
			ParentAgentID:   parentOpts.ParentAgentID,
			ParentIsUser:    parentOpts.ParentIsUser,
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

// openLog opens sess's http log writer, creating its file under the
// recorder's root. A nil Recorder (capture off) is a no-op; a failed open
// is logged, never a reason to fail the run. Call it once the session row
// exists, so every session that exists gets a file.
func (r *Runner) openLog(sess store.Session) {
	if r.Recorder == nil {
		return
	}
	if err := r.Recorder.Open(sess.ID, time.Now()); err != nil {
		log.Printf("session: open http log for %s: %v", sess.ID, err)
	}
}

// closeLog closes sessionID's http log writer, finalising its gzip member.
// A nil Recorder or a session that was never opened is a no-op.
func (r *Runner) closeLog(sessionID string) {
	if r.Recorder == nil {
		return
	}
	if err := r.Recorder.Close(sessionID); err != nil {
		log.Printf("session: close http log for %s: %v", sessionID, err)
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

// maxCompleteRejections is how many consecutive Complete calls may come back
// with the same validation error before the run ends on that error instead of
// letting the model keep rephrasing. Complete's schema failure deliberately
// does not end the run — the model gets to correct it (docs/TOOLS.md) — but a
// model that has not corrected it in three identical attempts is not
// correcting it at all: the phase 5 live run spent eleven consecutive
// sub-turns, 7% of its budget and 18% of everything it wrote all session,
// varying the payload's size rather than its shape, then ended on
// no_tool_calls asserting a harness bug that did not exist
// (docs/reviews/sess-bb6c0ed564ddae573c3b1832cb3981f4.md). Three is the point
// where the loop stops being a correction opportunity and starts being a
// budget leak. Only an identical message counts: a different error is the
// model making progress toward a different failure, and the count restarts.
const maxCompleteRejections = 3

// abandonRun ends a run that cannot get its Complete result past the schema.
// The rejection is recorded as an error event so the transcript says why the
// run stopped where it did, and the session is marked failed rather than ok:
// the requester asked for a schema-validated payload and there is none, which
// is not the same outcome as a run that simply never called Complete.
func (r *Runner) abandonRun(ctx context.Context, sess store.Session, allEvents []store.Event,
	agg Usage, subTurn int, lastText, completeError string) (*RunResult, error) {

	msg := fmt.Sprintf("session: Complete rejected %d times running with the same error, ending the run: %s",
		maxCompleteRejections, completeError)
	appended, err := r.Store.AppendEvents(ctx, sess.ID, []store.EventInput{
		{Kind: store.KindError, Payload: store.ErrorPayload{Message: msg}},
	})
	if err == nil {
		r.mirrorAppend(sess, appended)
		r.publishEvents(sess, appended)
		allEvents = append(allEvents, appended...)
	}
	return r.finishRun(ctx, sess, allEvents, ReasonCompleteRejected, store.StatusFailed,
		lastText, nil, "", "", agg, subTurn)
}

// ReasonCompleteRejected is run_finished's reason for a run abandonRun ended.
// The worker pool reads it off RunResult to classify the queue result, so it
// is named rather than a literal in two packages.
const ReasonCompleteRejected = "complete_rejected"

// finishRun records run_finished, updates the session's terminal status and
// complete_status, and rewrites the mirror's session.json one last time.
func (r *Runner) finishRun(ctx context.Context, sess store.Session, allEvents []store.Event,
	reason, sessionStatus, text string, result json.RawMessage, summary, completeStatus string,
	agg Usage, subTurns int) (*RunResult, error) {

	payload := store.RunFinishedPayload{Reason: reason, Text: text, Result: result, Summary: summary, Status: completeStatus}
	appended, err := r.Store.AppendEvents(ctx, sess.ID, []store.EventInput{{Kind: store.KindRunFinished, Payload: payload}})
	if err != nil {
		return &RunResult{SessionID: sess.ID, Status: store.StatusFailed, SubTurns: subTurns, Usage: agg, CompleteStatus: completeStatus, Reason: reason}, err
	}
	r.mirrorAppend(sess, appended)
	r.publishEvents(sess, appended)
	allEvents = append(allEvents, appended...)

	finished := time.Now().UTC()
	if err := r.Store.FinishSession(ctx, sess.ID, sessionStatus, completeStatus, summary, &finished); err != nil {
		return &RunResult{SessionID: sess.ID, Status: sessionStatus, SubTurns: subTurns, Usage: agg, CompleteStatus: completeStatus, Reason: reason}, err
	}
	r.closeLog(sess.ID)
	if updated, err := r.Store.GetSession(ctx, sess.ID); err == nil {
		r.mirrorUpdateSession(updated)
		r.publishState(ctx, updated)
	}

	return &RunResult{
		SessionID: sess.ID, Status: sessionStatus, Text: text, Result: result, Summary: summary,
		SubTurns: subTurns, Usage: agg, CompleteStatus: completeStatus, Reason: reason,
	}, nil
}

// fail records an error event and marks the session terminal. It always
// returns a non-nil error alongside a best-effort RunResult.
//
// The terminal bookkeeping runs on a fresh, bounded context rather than the
// run's own: a stop or a deadline is exactly what often ends a run this way,
// and the run's ctx is then already cancelled — a row update made through it
// failed silently before phase 5's live stop test, leaving the session
// running forever with the page stuck on RUNNING while the queue result
// already said cancelled. A run ended by a cancellation is marked cancelled
// (the status the stop escalation and the session list both use); any other
// failure marks the session failed.
func (r *Runner) fail(ctx context.Context, sess store.Session, allEvents []store.Event, subTurns int, agg Usage, cause error) (*RunResult, error) {
	terminalCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	appended, appendErr := r.Store.AppendEvents(terminalCtx, sess.ID, []store.EventInput{
		{Kind: store.KindError, Payload: store.ErrorPayload{Message: cause.Error()}},
	})
	if appendErr == nil {
		r.mirrorAppend(sess, appended)
		r.publishEvents(sess, appended)
		allEvents = append(allEvents, appended...)
	}

	status := store.StatusFailed
	switch ctx.Err() {
	case context.Canceled:
		// An operator's stop: the same status the force-finish escalation
		// writes, so a soft stop and a hard one agree on the row.
		status = store.StatusCancelled
	case context.DeadlineExceeded:
		// The run's own deadline expired: the queue result classifies the
		// same way (pool.classify's deadline_exceeded), and the session list
		// has a TIMEOUT badge for it.
		status = store.StatusTimeout
	}
	finished := time.Now().UTC()
	_ = r.Store.UpdateSessionStatus(terminalCtx, sess.ID, status, &finished)
	r.closeLog(sess.ID)
	if updated, err := r.Store.GetSession(terminalCtx, sess.ID); err == nil {
		r.mirrorUpdateSession(updated)
		r.publishState(terminalCtx, updated)
	}

	return &RunResult{SessionID: sess.ID, Status: status, SubTurns: subTurns, Usage: agg}, cause
}

// executeToolCalls runs every call concurrently and returns outcomes in the
// same order as calls, regardless of completion order — the appender is
// what must preserve tool_calls order, not the execution itself
// (docs/TOOLS.md). The four Task tools are the exception and run
// synchronously, in calls order, interleaved with the goroutine spawning:
// TaskCreate mints ids from a counter in execution order, and the store's
// plan replay (internal/store/status.go) and the frontend both reconstruct
// those ids by walking the event log in the same calls order, so letting
// two TaskCreate calls race for the executor's lock would hand the model
// ids nobody else can reconstruct. A Bash call gets a live stdout sink
// wired through its context so the browser can show output as it happens
// instead of only on completion (docs/DESIGN.md §5.2); every other tool
// runs exactly as before.
func (r *Runner) executeToolCalls(ctx context.Context, sess store.Session, executor *tools.Executor, calls []deepseek.AssembledToolCall) []tools.Outcome {
	outcomes := make([]tools.Outcome, len(calls))
	var wg sync.WaitGroup
	for i, c := range calls {
		if isTaskFamily(c.Name) {
			call := deepseek.ToolCall{
				ID:   c.ID,
				Type: "function",
				Function: deepseek.ToolCallFunc{
					Name:      c.Name,
					Arguments: c.Arguments,
				},
			}
			outcomes[i] = executor.Execute(httplog.WithSessionID(ctx, sess.ID), call)
			continue
		}
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
			callCtx := httplog.WithSessionID(ctx, sess.ID)
			if r.Hub != nil && c.Name == "Bash" {
				callCtx = tools.WithStdoutSink(callCtx, r.stdoutSink(ctx, sess, c.ID))
			}
			outcomes[i] = executor.Execute(callCtx, call)
		}(i, c)
	}
	wg.Wait()
	return outcomes
}

// isTaskFamily reports whether name is one of the four Task tools that
// execute synchronously in calls order rather than in a goroutine, so the
// ids TaskCreate mints never diverge from what the event-log replay
// reconstructs (see executeToolCalls).
func isTaskFamily(name string) bool {
	switch name {
	case "TaskCreate", "TaskGet", "TaskList", "TaskUpdate":
		return true
	}
	return false
}

// persistLiveState writes this sub-turn's recent-tool-call roll to the
// session row (docs/WEB-REDESIGN.md phase 3). The runner calls it where it
// already appends the tool_call events — the one component that sees every
// tool call and already holds the store handle — rather than inside
// tools.Executor, which deliberately does not import internal/store. The
// plan column is written separately by persistTaskState, after the tool
// calls have run, because a TaskCreate/TaskUpdate's effect (minted ids,
// patched fields) only exists once the handler actually executes.
// TaskCreate and TaskUpdate themselves are not rolled into the recent
// calls: the plan panel already says what they said. TaskGet and TaskList
// are non-mutating reads, so they roll like any other tool.
func (r *Runner) persistLiveState(ctx context.Context, sess store.Session, calls []deepseek.AssembledToolCall) {
	var recent []store.RecentToolCall
	for _, c := range calls {
		if c.Name == "TaskCreate" || c.Name == "TaskUpdate" {
			continue
		}
		recent = append(recent, store.RecentToolCall{
			Name:      c.Name,
			Arguments: c.Arguments,
			CreatedAt: time.Now().UTC(),
		})
	}
	if len(recent) == 0 {
		return
	}
	if err := r.Store.UpdateSessionLiveState(ctx, sess.ID, "", recent); err != nil {
		log.Printf("session: persist live state for %s: %v", sess.ID, err)
	}
}

// persistTaskState writes this sub-turn's plan to the session row, once the
// tool calls have actually run. It is a no-op unless the sub-turn carried a
// TaskCreate or TaskUpdate — the only two tools that mutate the plan — so a
// TaskGet/TaskList-only sub-turn never clobbers the row, and the
// recent-tool-call roll for this sub-turn was already handled by
// persistLiveState. The snapshot comes from executor.Todos() after
// execution, the only source of truth for minted ids and applied patches;
// UpdateSessionLiveState is called with a nil calls argument so this write
// touches the plan column only.
func (r *Runner) persistTaskState(ctx context.Context, sess store.Session, executor *tools.Executor, calls []deepseek.AssembledToolCall) {
	mutates := false
	for _, c := range calls {
		if c.Name == "TaskCreate" || c.Name == "TaskUpdate" {
			mutates = true
			break
		}
	}
	if !mutates {
		return
	}
	plan := planSnapshot(executor.Todos())
	if err := r.Store.UpdateSessionLiveState(ctx, sess.ID, plan, nil); err != nil {
		log.Printf("session: persist task state for %s: %v", sess.ID, err)
		return
	}
	// sess is a copy passed in by the caller; its Plan field predates the
	// write above (it was reloaded, if at all, before this sub-turn's
	// TaskCreate/TaskUpdate ran). Set it directly from the value just
	// written rather than reloading from the store again — publishState
	// only reads this copy, so mutating it here is enough to avoid the same
	// stale-publish bug turn.go's reload fixes for the roll.
	sess.Plan = plan
	r.publishState(ctx, sess)
}

// planSnapshot renders the executor's current plan as the JSON todos array
// the session row's plan column stores — store.StatusTodo, the same shape
// the status endpoint replays. The conversion is explicit field by field
// because there is no single raw-arguments source of truth anymore: the
// plan lives in the executor, mutated by TaskCreate and TaskUpdate.
func planSnapshot(todos []tools.Todo) string {
	out := make([]store.StatusTodo, len(todos))
	for i, t := range todos {
		out[i] = store.StatusTodo{
			TaskID:      t.TaskID,
			Subject:     t.Subject,
			Description: t.Description,
			Status:      t.Status,
			ActiveForm:  t.ActiveForm,
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return string(b)
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
