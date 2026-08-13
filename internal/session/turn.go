package session

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/mrgeoffrich/deepseek-harness/internal/cache"
	"github.com/mrgeoffrich/deepseek-harness/internal/fold"
	"github.com/mrgeoffrich/deepseek-harness/internal/httplog"
	"github.com/mrgeoffrich/deepseek-harness/internal/hub"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
	"github.com/mrgeoffrich/deepseek-harness/internal/tools"
	"github.com/mrgeoffrich/deepseek-harness/internal/wire"
)

// subTurnOutcome is what runSubTurn learned, folded down to what the loop
// in Run needs to decide what happens next.
type subTurnOutcome struct {
	usagePayload store.UsagePayload
	hasToolCalls bool
	completed    bool
	payload      tools.CompletePayload
	text         string
	// completeError is the message a rejected Complete came back with, and
	// "" when this sub-turn had no rejected Complete. The loop in Run
	// compares it across sub-turns to notice a model retrying an identical
	// rejection rather than correcting it (see maxCompleteRejections).
	completeError string
}

// steerBatchSize caps how many pending steers one sub-turn boundary applies.
// The loop reads pending steers exactly once per sub-turn
// (docs/RUN-CONTROL.md "How the loop picks one up"), so a burst of steers is
// delivered over a few sub-turns rather than ballooning a single request,
// and the per-sub-turn store read is bounded.
const steerBatchSize = 8

// steerPollInterval is how often the first-steer wait re-checks the log.
// The wait is for a person typing a first message, so a one-second poll is
// the human scale the feature is built for — nothing sub-second is gained.
const steerPollInterval = time.Second

// liveFlushInterval is how long text accumulates before the sink publishes
// it. Reading is the consumer, so this is a legibility figure rather than a
// latency one: ten frames a second already looks continuous, and publishing
// per token would put thousands of frames through a hub whose subscriber
// buffer is 256 and whose overflow rule is to drop the subscriber
// (internal/hub). Coalescing is what keeps a watching tab attached.
const liveFlushInterval = 100 * time.Millisecond

// liveSink publishes model output to the hub as it streams, coalesced into
// at most one frame per channel per liveFlushInterval. Nothing it sends is
// stored: the sub-turn's real reasoning_delta and content_delta events are
// committed with the rest of the batch when the response completes, and a
// browser that reconnects rebuilds from those. See hub.LiveDelta for why
// this exists at all.
//
// A nil *liveSink is a working no-op, which is what a Runner with no Hub
// gets — the CLI, and every test that does not assert on streaming.
type liveSink struct {
	hub       *hub.Hub
	sessionID string
	subTurn   int

	// Only the stream goroutine touches these: one liveSink belongs to one
	// r.stream call, and the deltas it coalesces arrive on that one
	// goroutine's range over the event channel.
	reasoning strings.Builder
	content   strings.Builder
	lastFlush time.Time
}

func newLiveSink(h *hub.Hub, sessionID string, subTurn int) *liveSink {
	if h == nil {
		return nil
	}
	return &liveSink{hub: h, sessionID: sessionID, subTurn: subTurn, lastFlush: time.Now()}
}

// add buffers one delta and publishes if the interval has elapsed.
func (s *liveSink) add(channel, text string) {
	if s == nil || text == "" {
		return
	}
	switch channel {
	case hub.ChannelReasoning:
		s.reasoning.WriteString(text)
	case hub.ChannelContent:
		s.content.WriteString(text)
	}
	if time.Since(s.lastFlush) >= liveFlushInterval {
		s.flush()
	}
}

// flush publishes whatever has accumulated. Reasoning goes first: the model
// thinks before it answers, so that is the order the text was produced in
// and the order a reader expects to watch it appear.
func (s *liveSink) flush() {
	if s == nil {
		return
	}
	if text := s.reasoning.String(); text != "" {
		s.hub.PublishLive(s.sessionID, hub.LiveDelta{SubTurn: s.subTurn, Channel: hub.ChannelReasoning, Text: text})
		s.reasoning.Reset()
	}
	if text := s.content.String(); text != "" {
		s.hub.PublishLive(s.sessionID, hub.LiveDelta{SubTurn: s.subTurn, Channel: hub.ChannelContent, Text: text})
		s.content.Reset()
	}
	s.lastFlush = time.Now()
}

// waitForFirstSteer blocks until at least one steer_message exists past seq
// 0 — the first sub-turn boundary will then apply it — or ctx ends. It is
// the whole "the browser-created run sits claimed with zero sub-turns until
// the operator types the first message" mechanism (docs/RUN-CONTROL.md "The
// frontend"): the steer is committed by the HTTP handler into the same
// store this polls, so there is no channel to wire and no coupling to
// httpapi. ctx carries the request deadline and the stop cancellation, so
// the wait is bounded and stoppable exactly like any other part of the run.
func (r *Runner) waitForFirstSteer(ctx context.Context, sessionID string) error {
	ticker := time.NewTicker(steerPollInterval)
	defer ticker.Stop()
	for {
		steers, err := r.Store.SteerMessagesAfter(ctx, sessionID, 0, 1)
		if err != nil {
			return fmt.Errorf("session: wait for first steer: %w", err)
		}
		if len(steers) > 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// pickUpSteers is the whole steering mechanism (docs/RUN-CONTROL.md "How the
// loop picks one up"): one store read at a sub-turn boundary — nothing
// mid-tool-call, nothing that blocks, no polling. It reads the steer_message
// events past the applied high-water mark, appends one steer_applied per
// steer as a single AppendEvents batch (so the mirror and the hub see one
// batch), pushes them onto allEvents so the fold that follows includes them,
// and advances appliedSeq. Steers arrive in seq order, so two steers fold to
// two user messages in the order they were sent. The high-water mark only
// ever moves forward by steers this call applied, so a steer is never
// delivered twice.
func (r *Runner) pickUpSteers(ctx context.Context, sess store.Session, allEvents *[]store.Event, appliedSeq *int64, subTurn int) error {
	steers, err := r.Store.SteerMessagesAfter(ctx, sess.ID, *appliedSeq, steerBatchSize)
	if err != nil {
		return fmt.Errorf("session: read pending steers: %w", err)
	}
	if len(steers) == 0 {
		return nil
	}

	inputs := make([]store.EventInput, 0, len(steers))
	for _, s := range steers {
		var p store.SteerMessagePayload
		if err := json.Unmarshal(s.Payload, &p); err != nil {
			return fmt.Errorf("session: decode steer_message at seq %d: %w", s.Seq, err)
		}
		inputs = append(inputs, store.EventInput{Kind: store.KindSteerApplied, Payload: store.SteerAppliedPayload{
			SourceSeq: s.Seq, Text: p.Text, SubTurn: subTurn,
		}})
	}
	appended, err := r.Store.AppendEvents(ctx, sess.ID, inputs)
	if err != nil {
		return fmt.Errorf("session: append steer_applied: %w", err)
	}
	r.mirrorAppend(sess, appended)
	r.publishEvents(sess, appended)
	*allEvents = append(*allEvents, appended...)
	// steers are in seq order and the steer_applied events were appended in
	// that order, so the last one carries the new high-water mark.
	*appliedSeq = steers[len(steers)-1].Seq
	return nil
}

// runSubTurn folds the log, sends one request, and commits the result. The
// whole sub-turn — deltas, tool calls, the finish marker, and usage —
// commits as one store.AppendEvents batch, so a crash mid-stream leaves no
// half-written turn behind: a resumed session either has the whole turn or
// none of it (docs/DESIGN.md §4.5, §4.8).
func (r *Runner) runSubTurn(ctx context.Context, sess store.Session, allEvents *[]store.Event, opts RunOptions,
	executor *tools.Executor, detector *cache.Detector, subTurn int, appliedSeq *int64,
	reminders *reminderState, contextTokens int) (subTurnOutcome, error) {

	// Pending steers are read once per sub-turn, before the fold, and folded
	// in as user messages at the tail — after the previous sub-turn's tool
	// round, never mid-call. A steer sent while a long tool call is running
	// reaches the model only here, at the next natural boundary, which is
	// what keeps §4.6's stalling problem closed.
	if err := r.emitDueReminder(ctx, sess, allEvents, reminders, subTurn, contextTokens); err != nil {
		return subTurnOutcome{}, err
	}
	if err := r.pickUpSteers(ctx, sess, allEvents, appliedSeq, subTurn); err != nil {
		return subTurnOutcome{}, err
	}

	messages, err := fold.Fold(sess, *allEvents)
	if err != nil {
		return subTurnOutcome{}, fmt.Errorf("session: fold: %w", err)
	}

	// The deliberate-churn debug hook (RunOptions.DebugChurnAtSubTurn):
	// break this one request's shared prefix on purpose so the diagnostic
	// below has a real divergence to name. messages[1] is the opening user
	// message, the earliest content that varies per session. The mutation
	// only touches the copy sent on the wire; *allEvents, and therefore
	// every later fold, is untouched.
	if opts.DebugChurnAtSubTurn > 0 && opts.DebugChurnAtSubTurn == subTurn && len(messages) > 1 {
		messages = cache.Mutate(messages, 1)
	}

	// turn_started is committed on its own, before the request, and not with
	// the batch that follows. AppendEvents stamps one instant across a batch,
	// so committing it with the rest gave it the time the model *finished* —
	// a reader had to know that to date anything in a sub-turn, and the
	// transcript's live turn measured its own age from it and always got
	// ~0 (docs/reviews/sess-bb6c0ed564ddae573c3b1832cb3981f4.md). Its own
	// append costs one transaction per sub-turn and makes the timestamp mean
	// what it says. It also tells a watching browser the sub-turn is in
	// flight, which is what the live states render against.
	//
	// The consequence to know: a sub-turn whose request fails leaves a
	// turn_started with no turn_finished. The fold treats it as a marker, the
	// sub-turn count includes it, and the transcript shows a live turn until
	// the run's terminal event lands — which fail() and finishRun() always
	// append.
	started, err := r.Store.AppendEvents(ctx, sess.ID, []store.EventInput{
		{Kind: store.KindTurnStarted, Payload: store.TurnStartedPayload{SubTurn: subTurn}},
	})
	if err != nil {
		return subTurnOutcome{}, fmt.Errorf("session: commit turn_started for sub-turn %d: %w", subTurn, err)
	}
	r.mirrorAppend(sess, started)
	r.publishEvents(sess, started)
	*allEvents = append(*allEvents, started...)

	// streamStart brackets the request(s) below. The elapsed figure is the
	// wall time the run waited on the API.
	streamStart := time.Now()
	live := newLiveSink(r.Hub, sess.ID, subTurn)
	reasoning, content, assembler, finishReason, usage, err := r.stream(httplog.WithSessionID(ctx, sess.ID), sess.Model, messages, opts.Effort, opts.Thinking, opts.MaxTokens, live)
	if err != nil {
		return subTurnOutcome{}, fmt.Errorf("session: sub-turn %d: %w", subTurn, err)
	}

	// Reasoning spends max_tokens before the answer starts, so an
	// undersized budget is billed in full and returns nothing. Retry once
	// at double the budget rather than treating it as a hard failure
	// (docs/OBSERVED.md, docs/MODELS.md).
	//
	// The starved attempt's usage is kept rather than overwritten. Both
	// requests are billed, so dropping the first understates the run's cost
	// by however much reasoning it burned — which, this failure mode being
	// what it is, is the whole of an exhausted max_tokens budget.
	var starved *wire.Usage
	if r.clientFor(sess.Model).IsReasoningStarved(finishReason, content) && len(assembler.Finalize()) == 0 {
		starved = usage
		reasoning, content, assembler, finishReason, usage, err = r.stream(ctx, sess.Model, messages, opts.Effort, opts.Thinking, opts.MaxTokens*2, live)
		if err != nil {
			return subTurnOutcome{}, fmt.Errorf("session: sub-turn %d retry: %w", subTurn, err)
		}
	}
	// Elapsed covers the retry too; the run waited on both requests.
	elapsedMs := time.Since(streamStart).Milliseconds()

	toolCalls := assembler.Finalize()
	// A misplaced brace in a large arguments object costs a whole sub-turn
	// otherwise, and the repair is a single character (docs/OBSERVED.md).
	// It happens here, before the events are appended, so the event log,
	// the browser, the history the next request replays, and the executor
	// all see one set of arguments rather than the store disagreeing with
	// what ran. The model's own bytes are still on disk in the HTTP log,
	// which is where a question about what it actually emitted belongs.
	for i := range toolCalls {
		repaired, ok := r.clientFor(sess.Model).RepairArguments(finishReason, toolCalls[i].Arguments)
		if !ok {
			continue
		}
		log.Printf("session: repaired malformed arguments for %s in %s sub-turn %d",
			toolCalls[i].Name, sess.ID, subTurn)
		toolCalls[i].Arguments = repaired
	}

	var inputs []store.EventInput
	// The discarded attempt is committed ahead of everything the retry
	// produced, which is the order the two requests happened in. It carries
	// no churn report: the detector observes the request whose prefix the
	// next turn actually builds on, and both attempts sent the same
	// messages, so observing twice would double-count one prefix.
	if starved != nil {
		inputs = append(inputs, store.EventInput{Kind: store.KindUsage,
			Payload: r.buildUsagePayload(sess.Model, starved, nil, nil, subTurn, 1)})
	}
	if reasoning != "" {
		inputs = append(inputs, store.EventInput{Kind: store.KindReasoningDelta, Payload: store.ReasoningDeltaPayload{Text: reasoning}})
	}
	if content != "" {
		inputs = append(inputs, store.EventInput{Kind: store.KindContentDelta, Payload: store.ContentDeltaPayload{Text: content}})
	}
	for i, tc := range toolCalls {
		inputs = append(inputs, store.EventInput{Kind: store.KindToolCall, Payload: store.ToolCallPayload{
			Index: i, ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments,
		}})
	}
	inputs = append(inputs, store.EventInput{Kind: store.KindTurnFinished, Payload: store.TurnFinishedPayload{
		SubTurn: subTurn, FinishReason: finishReason, ElapsedMs: elapsedMs,
	}})

	// Attempt stays 0 unless the retry above ran, so an ordinary sub-turn's
	// usage event is unchanged but for its new SubTurn.
	attempt := 0
	if starved != nil {
		attempt = 2
	}
	usagePayload := r.buildUsagePayload(sess.Model, usage, messages, detector, subTurn, attempt)
	inputs = append(inputs, store.EventInput{Kind: store.KindUsage, Payload: usagePayload})

	appended, err := r.Store.AppendEvents(ctx, sess.ID, inputs)
	if err != nil {
		return subTurnOutcome{}, fmt.Errorf("session: commit sub-turn %d: %w", subTurn, err)
	}
	r.mirrorAppend(sess, appended)
	r.publishEvents(sess, appended)
	*allEvents = append(*allEvents, appended...)
	// Persist the plan and recent-tool-call roll here, beside the tool_call
	// events just appended, so the state publish that follows carries the
	// new plan on the same sub-turn. The
	// runner sees every tool call and holds the store handle; tools.Executor
	// never does.
	r.persistLiveState(ctx, sess, toolCalls)
	// sess is the value runSubTurn was called with and was never updated
	// in memory with the plan/roll persistLiveState just wrote, so
	// publishing it as-is would push a session_state event that reverts
	// the browser's in-flight card to whatever it looked like before this
	// sub-turn. Reload it, the same way finishRun/fail/compact do before
	// their own publishState calls.
	if updated, err := r.Store.GetSession(ctx, sess.ID); err == nil {
		sess = updated
	}
	r.publishState(ctx, sess)

	if progress := progressFunc(r, opts); progress != nil {
		progress(SubTurnProgress{
			SessionID: sess.ID, SubTurn: subTurn, Model: sess.Model,
			ToolCalls: toolCallNames(toolCalls), Usage: usagePayload,
			Churned: usagePayload.ChurnPointIndex != nil,
		})
	}

	if len(toolCalls) == 0 {
		return subTurnOutcome{usagePayload: usagePayload, text: content}, nil
	}

	outcomes := r.executeToolCalls(ctx, sess, executor, toolCalls)
	// The plan column can only be persisted after the calls ran: TaskCreate
	// mints ids and TaskUpdate patches fields inside the handlers, so
	// executor.Todos() below reflects this sub-turn's mutations. The roll was
	// already handled by persistLiveState before execution.
	r.persistTaskState(ctx, sess, executor, toolCalls)
	toolInputs := make([]store.EventInput, 0, len(outcomes))
	var completePayload tools.CompletePayload
	completed := false
	completeError := ""
	for i, oc := range outcomes {
		switch {
		case oc.Denied:
			toolInputs = append(toolInputs, store.EventInput{Kind: store.KindToolDenied, Payload: store.ToolDeniedPayload{
				ToolCallID: toolCalls[i].ID, Name: oc.Name, Rule: oc.Rule, Content: oc.Result.Content,
			}})
		default:
			toolInputs = append(toolInputs, store.EventInput{Kind: store.KindToolResult, Payload: store.ToolResultPayload{
				ToolCallID: toolCalls[i].ID, Name: oc.Name, Content: oc.Result.Content,
				IsError: oc.Result.IsError, Truncated: oc.Result.Truncated,
				Diff: oc.Result.Diff, ChildSessionID: oc.Result.ChildSessionID,
			}})
			// A ReviewScreenshot call bills separately from this sub-turn's
			// DeepSeek request; its usage rides home on the tool result and
			// is committed as its own usage event, so the session's cost
			// total covers Gemini the same way it covers DeepSeek
			// (docs/DESIGN.md §4.9, SessionUsageSummaries sums every usage
			// event). The cost and token mapping are already computed by
			// the tool; the runner only stamps the sub-turn it happened in.
			if oc.Result.GeminiUsage != nil {
				g := *oc.Result.GeminiUsage
				g.SubTurn = subTurn
				toolInputs = append(toolInputs, store.EventInput{Kind: store.KindUsage, Payload: g})
			}
		}
		if oc.IsComplete && !completed {
			completed = true
			completePayload = oc.CompletePayload
		}
		if oc.Name == "Complete" && !oc.IsComplete && oc.Result.IsError && completeError == "" {
			completeError = oc.Result.Content
		}
	}

	appended2, err := r.Store.AppendEvents(ctx, sess.ID, toolInputs)
	if err != nil {
		return subTurnOutcome{}, fmt.Errorf("session: commit tool results for sub-turn %d: %w", subTurn, err)
	}
	r.mirrorAppend(sess, appended2)
	r.publishEvents(sess, appended2)
	*allEvents = append(*allEvents, appended2...)

	return subTurnOutcome{
		usagePayload:  usagePayload,
		hasToolCalls:  true,
		completed:     completed,
		payload:       completePayload,
		text:          content,
		completeError: completeError,
	}, nil
}

// buildUsagePayload turns the API's usage figures into a stored
// UsagePayload, including cost and the churn diagnostic. detector.Observe
// must be called exactly once per sub-turn, in order, which is why this is
// folded into the single place runSubTurn calls per turn.
// buildUsagePayload turns one request's usage into its store event. A nil
// detector skips the churn report, for an attempt whose prefix the next turn
// will not build on.
func (r *Runner) buildUsagePayload(model string, usage *wire.Usage, requestMessages []wire.Message,
	detector *cache.Detector, subTurn, attempt int) store.UsagePayload {

	if usage == nil {
		return store.UsagePayload{SubTurn: subTurn, Attempt: attempt}
	}
	reasoningTokens := 0
	if usage.CompletionTokensDetails != nil {
		reasoningTokens = usage.CompletionTokensDetails.ReasoningTokens
	}
	// How usage becomes cache-hit and cache-miss counts is the provider's
	// decision — DeepSeek reports the two figures separately, Kimi K3 a
	// single cached_tokens — so the split comes through the seam
	// (client.go, docs/KIMI-INTEGRATION.md §2).
	cacheHit, cacheMiss := r.clientFor(model).UsageSplit(usage)
	cost := 0.0
	if r.Prices != nil {
		if c, err := r.Prices.Cost(model, cacheHit, cacheMiss, usage.CompletionTokens); err == nil {
			cost = c
		}
	}
	var report cache.Report
	if detector != nil {
		report = detector.Observe(requestMessages, *usage)
	}
	return store.UsagePayload{
		SubTurn:               subTurn,
		Attempt:               attempt,
		PromptTokens:          usage.PromptTokens,
		PromptCacheHitTokens:  cacheHit,
		PromptCacheMissTokens: cacheMiss,
		CompletionTokens:      usage.CompletionTokens,
		ReasoningTokens:       reasoningTokens,
		CostUSD:               cost,
		ExpectedMissTokens:    report.ExpectedMissTokens,
		ChurnPointIndex:       report.ChurnPointIndex,
	}
}

// stream sends one request and collects the full response from the event
// channel. The loop states its intent — model, messages, effort, whether to
// think, the token ceiling, the tools — and the Client implementation turns
// that into its provider's request shape; serialisation happens once inside
// that implementation and any HTTP-level retry resends those identical
// bytes (docs/CACHE.md). This function does not re-serialise between
// attempts.
func (r *Runner) stream(ctx context.Context, model string, messages []wire.Message, effort string, thinking bool, maxTokens int, live *liveSink) (
	reasoning, content string, assembler *wire.ToolCallAssembler, finishReason string, usage *wire.Usage, err error) {

	intent := wire.ChatIntent{
		Model:     model,
		Messages:  messages,
		Effort:    effort,
		Thinking:  thinking,
		MaxTokens: maxTokens,
		Tools:     tools.Definitions(),
	}

	release, err := r.acquireModelSlot(ctx, model)
	if err != nil {
		return "", "", nil, "", nil, err
	}
	defer release()

	events, err := r.clientFor(model).StreamChatCompletion(ctx, intent)
	if err != nil {
		return "", "", nil, "", nil, err
	}

	var reasoningBuf, contentBuf strings.Builder
	assembler = wire.NewToolCallAssembler()
	var streamErr error
	// Whatever this loop accumulates is committed as one batch by the
	// caller; the sink is what a watching browser sees in the meantime.
	// Flushed unconditionally on the way out so the tail of the response is
	// not left sitting in the builder — including on the error paths, where
	// the text streamed so far is all anyone will get.
	defer live.flush()
	for ev := range events {
		switch ev.Type {
		case wire.EventReasoningDelta:
			reasoningBuf.WriteString(ev.Reasoning)
			live.add(hub.ChannelReasoning, ev.Reasoning)
		case wire.EventContentDelta:
			contentBuf.WriteString(ev.Content)
			live.add(hub.ChannelContent, ev.Content)
		case wire.EventToolCallDelta:
			assembler.Add(ev.ToolCall)
		case wire.EventFinish:
			finishReason = ev.FinishReason
		case wire.EventUsage:
			usage = ev.Usage
		case wire.EventError:
			streamErr = ev.Err
		}
	}
	if streamErr != nil {
		return "", "", nil, "", nil, streamErr
	}
	return reasoningBuf.String(), contentBuf.String(), assembler, finishReason, usage, nil
}

// emitDueReminder appends a reminder when the run's policy says the context
// has grown far enough since the last one. It writes a steer_applied directly
// rather than a steer_message an operator sent: there is no source event to
// link, so SourceSeq stays zero and the applied high-water mark ignores it.
//
// The reminder lands at the tail, before the fold, so it is the last thing in
// the conversation when the next request goes out and nothing earlier moves.
func (r *Runner) emitDueReminder(ctx context.Context, sess store.Session, allEvents *[]store.Event,
	reminders *reminderState, subTurn, contextTokens int) error {

	if reminders == nil || contextTokens <= 0 {
		return nil
	}
	text, role, ok := reminders.due(contextTokens)
	if !ok {
		return nil
	}
	appended, err := r.Store.AppendEvents(ctx, sess.ID, []store.EventInput{{
		Kind:    store.KindSteerApplied,
		Payload: store.SteerAppliedPayload{Text: text, SubTurn: subTurn, Role: role},
	}})
	if err != nil {
		return fmt.Errorf("session: append reminder: %w", err)
	}
	r.mirrorAppend(sess, appended)
	r.publishEvents(sess, appended)
	*allEvents = append(*allEvents, appended...)
	return nil
}
