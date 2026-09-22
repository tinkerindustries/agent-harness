package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/agentmeta"
	"github.com/mrgeoffrich/agent-harness/internal/attachment"
	"github.com/mrgeoffrich/agent-harness/internal/cache"
	"github.com/mrgeoffrich/agent-harness/internal/claudemd"
	"github.com/mrgeoffrich/agent-harness/internal/gemini"
	"github.com/mrgeoffrich/agent-harness/internal/promptvariant"
	"github.com/mrgeoffrich/agent-harness/internal/provider"
	"github.com/mrgeoffrich/agent-harness/internal/skills"
	"github.com/mrgeoffrich/agent-harness/internal/store"
	"github.com/mrgeoffrich/agent-harness/internal/tools"
	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// lifecycle.go: Create/FailSetup/Run and the loop that drives one run to a
// terminal result — the pre-created "creating" row, the promote-or-insert
// on Run, the sub-turn loop itself, subagent spawning, and the three ways a
// run ends (finishRun, fail, abandonRun).

// Create inserts the session row for a run whose workspace is still being
// prepared — the window between the worker minting the session id and the
// agent loop's first request. The row is created "creating" with the
// workspace path the worker is about to clone into and the metadata the
// request already carries (model, effort, task, title, description, phase,
// parent fields, permission mode, deny patterns); system_prompt and
// tool_schema stay empty because neither is resolvable until the run starts
// — Runner.Run promotes the row and writes them. publishState follows so the
// row reaches the session-list feed immediately. It lives on the Runner
// rather than in the worker because publishState needs the Hub and the
// usage/request-id lookups the Runner already holds, and the worker must not
// gain a hub dependency.
func (r *Runner) Create(ctx context.Context, opts RunOptions) error {
	sess := opts.session(store.StatusCreating, opts.Workspace)
	if err := r.Store.CreateSession(ctx, sess); err != nil {
		return fmt.Errorf("session: create session: %w", err)
	}
	curSess, err := r.Store.GetSession(ctx, sess.ID)
	if err != nil {
		return fmt.Errorf("session: reload created session: %w", err)
	}
	r.publishState(ctx, curSess)
	return nil
}

// FailSetup moves a "creating" session row to "failed" with finished_at,
// appends an error event so the session page shows why the run never
// started, and publishes the updated state. It is the worker's terminal
// bookkeeping for a run whose workspace could not be built (an attachment
// that vanished, a clone that was refused), and the counterpart of the
// promotion Run performs when preparation succeeds.
//
// The terminal bookkeeping runs on a fresh, bounded context rather than the
// caller's, the same rule fail follows: a stop or a deadline is exactly what
// often ends preparation this way, and the caller's ctx is then already
// cancelled. A row the stop escalation already marked cancelled refuses the
// update (UpdateSessionStatus returns ErrSessionCancelled) and is treated as
// success — the stop owns that row, and its cancelled result is on its way.
func (r *Runner) FailSetup(ctx context.Context, sessionID string, cause error) error {
	terminalCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sess, err := r.Store.GetSession(terminalCtx, sessionID)
	if err != nil {
		return fmt.Errorf("session: fail setup: load %s: %w", sessionID, err)
	}
	appended, appendErr := r.Store.AppendEvents(terminalCtx, sessionID, []store.EventInput{
		{Kind: store.KindError, Payload: store.ErrorPayload{Message: cause.Error()}},
	})
	if appendErr != nil {
		log.Printf("session: fail setup for %s: append error event: %v", sessionID, appendErr)
	} else {
		r.mirrorAppend(sess, appended)
		// Deferred for the reason finishRun defers its own: the error event
		// closes the stream, so the "failed" row below has to go out first
		// (publishTerminalEvents).
		defer r.publishTerminalEvents(sess, appended)
	}

	finished := time.Now().UTC()
	if err := r.Store.UpdateSessionStatus(terminalCtx, sessionID, store.StatusFailed, &finished); err != nil {
		if errors.Is(err, store.ErrSessionCancelled) {
			return nil
		}
		return fmt.Errorf("session: fail setup: mark %s failed: %w", sessionID, err)
	}
	if updated, err := r.Store.GetSession(terminalCtx, sessionID); err == nil {
		r.mirrorUpdateSession(updated)
		r.publishState(terminalCtx, updated)
	}
	return nil
}

// Run drives one session from creation to a terminal state: a response with
// no tool calls, a successful Complete call, a cancelled or expired context,
// or an unrecoverable error. No count of sub-turns ends it. The returned
// error is non-nil only for infrastructure failures (store or stream errors);
// a task the model gave up on is a normal RunResult, not an error.
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

	if opts.SessionID == "" {
		opts.SessionID = newID("sess")
	}
	sessID := opts.SessionID

	// MCP definitions are resolved once, here, for the whole run
	// (docs/MCP.md, "Resolution happens once per run"): the provider's
	// frozen array plus whatever servers are enabled right now, appended
	// after it so the built-in portion's own bytes never move. Every
	// request this run sends, and the schema stored on the session row,
	// come from this one resolution — never a second call to
	// tools.DefinitionsForVariant or r.MCP.Definitions later in the run.
	mcpDefs, mcpReadOnly := r.resolveMCPDefinitions(ctx, sessID)
	opts.Tools = tools.WithMCP(tools.DefinitionsForVariant(opts.Model, opts.PromptVariant), mcpDefs)

	// A Gemini session's tools go through gemini.LowerToolSchemas before
	// anything else sees them, so the row, the head and every request all
	// carry the lowered array — the agreement the tool-schema comment below
	// rests on. The Interactions API refuses JSON Schema's tuple form for
	// `items` and says only "syntax error in request body", taking every
	// other tool in the payload down with the offending one
	// (internal/gemini/schema.go). An MCP server is where one arrives: its
	// tools are declared by whoever wrote the server, against no constraint
	// this process imposes.
	// A model ModelFor rejects is not Gemini's, and takes the default path
	// here the same way it does everywhere else (docs/DESIGN.md §3.2).
	if name, _ := provider.ModelFor(opts.Model); name == provider.Gemini {
		lowered, err := gemini.LowerToolSchemas(opts.Tools)
		if err != nil {
			return nil, err
		}
		opts.Tools = lowered
	}

	policy := &tools.Policy{
		Mode:               opts.PermissionMode,
		Deny:               opts.Deny,
		MCPReadOnlyServers: mcpReadOnly,
	}
	executor, err := tools.NewExecutor(opts.Workspace, policy)
	if err != nil {
		return nil, err
	}
	// A background shell a Bash call started outlives the call itself; it
	// must not outlive the run. Close kills whatever is still running on
	// every return path, including the ones above this line's siblings
	// return early on error, none of which have started anything yet.
	defer executor.Close()
	executor.Client = r.clientFor(r.flashModel(ctx))
	executor.Prices = r.Prices
	executor.FlashModel = r.flashModel(ctx)
	executor.SeeImages = seesImages(opts.Model)
	executor.Gemini = r.Gemini
	executor.GeminiModel = r.GeminiModel
	executor.Settings = r.Settings
	executor.ExtraEnv = r.ToolEnv
	executor.EnvFilter = r.ToolEnvFilter
	executor.Timeouts = r.ToolTimeouts
	executor.ResultSchema = opts.ResultSchema
	executor.MCP = r.MCP
	executor.RG = r.RG

	// The schema stored on the session row is exactly the array every
	// request of this run sends — opts.Tools, resolved once above — so the
	// row, the head, and every request agree by construction rather than by
	// each re-deriving the same thing.
	toolSchema, err := json.Marshal(opts.Tools)
	if err != nil {
		return nil, fmt.Errorf("session: encode tool schema: %w", err)
	}

	sysPrompt, err := RenderSystemPromptFor(opts.Model, opts.PromptVariant)
	if err != nil {
		return nil, err
	}

	executor.RunSubagent = r.subagentRunner(sessID, opts, executor.Workspace)

	// The worker creates the row as "creating" before it prepares the
	// workspace, so a run is visible and stoppable from the moment it is
	// claimed. Run promotes that row to "running" — writing the four
	// columns that are only resolvable now that the run is starting — and
	// otherwise inserts the row exactly as it always has. Every caller that
	// does not clone (a resumed session, the Task subagent path,
	// compaction) has no pre-created row, so it always inserts, and none of
	// them should ever show "creating".
	if existing, err := r.Store.GetSession(ctx, sessID); err == nil && existing.Status == store.StatusCreating {
		if err := r.Store.PromoteSession(ctx, sessID, executor.Workspace, sysPrompt, toolSchema, opts.ResultSchema, mcpReadOnly); err != nil {
			return nil, fmt.Errorf("session: promote session: %w", err)
		}
	} else {
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("session: load session: %w", err)
		}
		sess := opts.session(store.StatusRunning, executor.Workspace)
		sess.SystemPrompt = sysPrompt
		sess.ToolSchema = toolSchema
		sess.MCPReadOnly = mcpReadOnly
		if err := r.Store.CreateSession(ctx, sess); err != nil {
			return nil, fmt.Errorf("session: create session: %w", err)
		}
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
	mcpBlock := RenderMCPBlock(opts.Tools, r.resolveMCPInstructions(ctx, sessID))
	opening := RenderOpeningMessage(executor.Workspace, opts.Prompt, opts.ResultSchema, claudeMD, catalogue, mcpBlock, opts.AttachmentNames)
	appended, err := r.Store.AppendEvents(ctx, sessID, []store.EventInput{
		{Kind: store.KindSessionStarted, Payload: store.SessionStartedPayload{
			OpeningMessage: opening,
			SkillCatalogue: catalogue,
			ClaudeMDBlock:  claudeMD,
			Task:           opts.Prompt,
			// The browser addresses each file on
			// GET /api/sessions/{id}/screenshot by exactly this path
			// (store.SessionStartedPayload.Attachments).
			Attachments: attachmentPaths(opts.AttachmentNames),
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

// attachmentPaths turns attachment names into the workspace paths they were
// materialised under, from the one spelling every package that touches them
// reads (internal/attachment.WorkspacePaths) — exactly the paths
// RenderAttachmentBlock lists and GET /api/sessions/{id}/screenshot
// resolves. Nil when there are none, so the payload field stays absent for a
// run without attachments.
func attachmentPaths(names []string) []string {
	return attachment.WorkspacePaths(names)
}

// runLoop iterates sub-turns from startSubTurn until the session reaches a
// terminal state. Both Run and Resume end by calling this; they differ only
// in how curSess, allEvents, executor, and detector got built — a fresh
// session versus one reloaded from the store — and in where their own
// numbering starts.
func (r *Runner) runLoop(ctx context.Context, curSess store.Session, allEvents []store.Event, opts RunOptions,
	executor *tools.Executor, detector *cache.Detector, startSubTurn int) (*RunResult, error) {

	var agg Usage
	var lastText string

	// The applied-steer high-water mark is derived from the log, once, when a
	// run starts or resumes — never carried in memory across runs, so Resume
	// and a compacted successor both recompute it and a steer is never
	// delivered twice (docs/RUN-CONTROL.md "How the loop picks one up"). The
	// one rough edge of the two-kind design, worth saying here rather than
	// leaving a later reader to work out: a compacted session is a NEW session
	// id with a fresh log, so steers applied before compaction live in the
	// summary that carried them forward, and unapplied ones stay attached to
	// the retired session — they are neither delivered nor lost.
	reminderPolicy := opts.ReminderPolicy
	if reminderPolicy == "" {
		reminderPolicy = promptvariant.ReminderPolicyFor(opts.PromptVariant)
	}
	reminders := newReminderState(reminderPolicy)
	contextTokens := 0

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
	// ends the run (no_tool_calls) before the operator can type, which a
	// live run exposed. Only the empty-prompt start waits: every
	// other ingress (publish, MCP, subagents, resume) names a task. The
	// wait is bounded by the run's own budget — ctx carries the request
	// deadline and the stop cancellation — so an abandoned empty run
	// expires at its deadline and a stop ends it immediately.
	//
	// !opts.Resuming is what keeps this from also catching a resume: a stop
	// that lands before the first turn_started commits leaves startSubTurn
	// at 1 on the resumed run too, and Resume never sets opts.Prompt
	// (RunOptions.session's doc comment), so both halves of this condition
	// can otherwise be true for a resume that has every right to run —
	// waiting on a steer that Resume already delivered as a session_started
	// event, not a steer_message, so it would never arrive.
	if !opts.Resuming && startSubTurn == 1 && opts.Prompt == "" {
		if err := r.waitForFirstSteer(ctx, curSess.ID); err != nil {
			return r.fail(ctx, curSess, allEvents, startSubTurn-1, agg, err)
		}
	}

	// Consecutive Complete rejections carrying the same message: see
	// maxCompleteRejections.
	lastCompleteError, completeRejections := "", 0

	for subTurn := startSubTurn; ; subTurn++ {
		outcome, err := r.runSubTurn(ctx, curSess, &allEvents, opts, executor, detector, subTurn, &appliedSeq, &reminders, contextTokens)
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

		contextTokens = outcome.usagePayload.PromptTokens

		if outcome.usagePayload.PromptTokens >= r.compactionThreshold(ctx, opts.Model) {
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
			Effort:          wire.EffortMax,
			Thinking:        true,
			MaxTokens:       20000,
			Workspace:       workspace,
			PermissionMode:  parentOpts.PermissionMode,
			Deny:            parentOpts.Deny,
			Prompt:          prompt,
			ParentID:        parentID,
			JobType:         agentmeta.JobTypeImplementation,
			ParentAgentType: parentOpts.ParentAgentType,
			ParentAgentID:   parentOpts.ParentAgentID,
			ParentIsUser:    parentOpts.ParentIsUser,
			// The Task tool's own description argument is exactly a short
			// label for the child's work, so it becomes the child's title —
			// truncated to the title cap rather than failing the subagent
			// call when a model supplies a long one. The child's Description
			// stays empty and its phase position is inherited from the
			// parent: a compacted continuation is the same job, and so is a
			// delegated slice of one.
			Title:       truncateTitle(description),
			Phase:       parentOpts.Phase,
			TotalPhases: parentOpts.TotalPhases,
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

// truncateTitle clips a model-supplied label down to the title word cap
// (agentmeta.MaxTitleWords): the first cap words, joined with single spaces
// so a newline or a run of whitespace cannot smuggle itself into a title.
// The alternative — letting a long Task description fail validation and
// abort the subagent call — would turn a cosmetic cap into a lost run.
func truncateTitle(s string) string {
	fields := strings.Fields(s)
	if len(fields) <= agentmeta.MaxTitleWords {
		return strings.Join(fields, " ")
	}
	return strings.Join(fields[:agentmeta.MaxTitleWords], " ")
}

// maxCompleteRejections is how many consecutive Complete calls may come back
// with the same validation error before the run ends on that error instead of
// letting the model keep rephrasing. Complete's schema failure deliberately
// does not end the run — the model gets to correct it (docs/TOOLS.md) — but a
// model that has not corrected it in three identical attempts is not
// correcting it at all: one live run spent eleven consecutive
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
	// Deferred, not called here: run_finished closes the session's stream, so
	// the terminal row below has to reach subscribers first
	// (publishTerminalEvents). The defer is what keeps that true on the error
	// return in between, which still owes the browser the event.
	defer r.publishTerminalEvents(sess, appended)
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
// once failed silently, leaving the session
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
		// Deferred for the reason finishRun defers its own: the error event
		// closes the stream, so the terminal row has to go out first
		// (publishTerminalEvents).
		defer r.publishTerminalEvents(sess, appended)
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
