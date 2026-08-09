package session

import (
	"context"
	"fmt"
	"strings"

	"github.com/mrgeoffrich/deepseek-harness/internal/cache"
	"github.com/mrgeoffrich/deepseek-harness/internal/deepseek"
	"github.com/mrgeoffrich/deepseek-harness/internal/fold"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
	"github.com/mrgeoffrich/deepseek-harness/internal/tools"
)

// subTurnOutcome is what runSubTurn learned, folded down to what the loop
// in Run needs to decide what happens next.
type subTurnOutcome struct {
	usagePayload store.UsagePayload
	hasToolCalls bool
	completed    bool
	payload      tools.CompletePayload
	text         string
}

// runSubTurn folds the log, sends one request, and commits the result. The
// whole sub-turn — deltas, tool calls, the finish marker, and usage —
// commits as one store.AppendEvents batch, so a crash mid-stream leaves no
// half-written turn behind: a resumed session either has the whole turn or
// none of it (docs/DESIGN.md §4.5, §4.8).
func (r *Runner) runSubTurn(ctx context.Context, sess store.Session, allEvents *[]store.Event, opts RunOptions,
	executor *tools.Executor, detector *cache.Detector, subTurn int) (subTurnOutcome, error) {

	messages, err := fold.Fold(sess, *allEvents)
	if err != nil {
		return subTurnOutcome{}, fmt.Errorf("session: fold: %w", err)
	}

	reasoning, content, assembler, finishReason, usage, err := r.stream(ctx, sess.Model, messages, opts.Effort, opts.Thinking, opts.MaxTokens)
	if err != nil {
		return subTurnOutcome{}, fmt.Errorf("session: sub-turn %d: %w", subTurn, err)
	}

	// Reasoning spends max_tokens before the answer starts, so an
	// undersized budget is billed in full and returns nothing. Retry once
	// at double the budget rather than treating it as a hard failure
	// (docs/OBSERVED.md, docs/MODELS.md).
	if deepseek.IsReasoningStarved(finishReason, content) && len(assembler.Finalize()) == 0 {
		reasoning, content, assembler, finishReason, usage, err = r.stream(ctx, sess.Model, messages, opts.Effort, opts.Thinking, opts.MaxTokens*2)
		if err != nil {
			return subTurnOutcome{}, fmt.Errorf("session: sub-turn %d retry: %w", subTurn, err)
		}
	}

	toolCalls := assembler.Finalize()

	inputs := []store.EventInput{{Kind: store.KindTurnStarted, Payload: store.TurnStartedPayload{SubTurn: subTurn}}}
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
	inputs = append(inputs, store.EventInput{Kind: store.KindTurnFinished, Payload: store.TurnFinishedPayload{FinishReason: finishReason}})

	usagePayload := r.buildUsagePayload(sess.Model, usage, messages, detector)
	inputs = append(inputs, store.EventInput{Kind: store.KindUsage, Payload: usagePayload})

	appended, err := r.Store.AppendEvents(ctx, sess.ID, inputs)
	if err != nil {
		return subTurnOutcome{}, fmt.Errorf("session: commit sub-turn %d: %w", subTurn, err)
	}
	r.mirrorAppend(sess, appended)
	*allEvents = append(*allEvents, appended...)
	r.mirrorTranscript(sess, *allEvents)

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

	outcomes := executeToolCalls(ctx, executor, toolCalls)
	toolInputs := make([]store.EventInput, 0, len(outcomes))
	var completePayload tools.CompletePayload
	completed := false
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
			}})
		}
		if oc.IsComplete && !completed {
			completed = true
			completePayload = oc.CompletePayload
		}
	}

	appended2, err := r.Store.AppendEvents(ctx, sess.ID, toolInputs)
	if err != nil {
		return subTurnOutcome{}, fmt.Errorf("session: commit tool results for sub-turn %d: %w", subTurn, err)
	}
	r.mirrorAppend(sess, appended2)
	*allEvents = append(*allEvents, appended2...)

	return subTurnOutcome{
		usagePayload: usagePayload,
		hasToolCalls: true,
		completed:    completed,
		payload:      completePayload,
		text:         content,
	}, nil
}

// buildUsagePayload turns the API's usage figures into a stored
// UsagePayload, including cost and the churn diagnostic. detector.Observe
// must be called exactly once per sub-turn, in order, which is why this is
// folded into the single place runSubTurn calls per turn.
func (r *Runner) buildUsagePayload(model string, usage *deepseek.Usage, requestMessages []deepseek.Message, detector *cache.Detector) store.UsagePayload {
	if usage == nil {
		return store.UsagePayload{}
	}
	reasoningTokens := 0
	if usage.CompletionTokensDetails != nil {
		reasoningTokens = usage.CompletionTokensDetails.ReasoningTokens
	}
	cost := 0.0
	if r.Prices != nil {
		if c, err := r.Prices.Cost(model, usage.PromptCacheHitTokens, usage.PromptCacheMissTokens, usage.CompletionTokens); err == nil {
			cost = c
		}
	}
	report := detector.Observe(requestMessages, *usage)
	return store.UsagePayload{
		PromptTokens:          usage.PromptTokens,
		PromptCacheHitTokens:  usage.PromptCacheHitTokens,
		PromptCacheMissTokens: usage.PromptCacheMissTokens,
		CompletionTokens:      usage.CompletionTokens,
		ReasoningTokens:       reasoningTokens,
		CostUSD:               cost,
		ExpectedMissTokens:    report.ExpectedMissTokens,
		ChurnPointIndex:       report.ChurnPointIndex,
	}
}

// stream sends one request and collects the full response from the event
// channel. Serialisation happens once inside Client.StreamChatCompletion
// and any HTTP-level retry resends those identical bytes
// (docs/CACHE.md); this function does not re-serialise between attempts.
func (r *Runner) stream(ctx context.Context, model string, messages []deepseek.Message, effort string, thinking bool, maxTokens int) (
	reasoning, content string, assembler *deepseek.ToolCallAssembler, finishReason string, usage *deepseek.Usage, err error) {

	thinkingType := deepseek.ThinkingDisabled
	if thinking {
		thinkingType = deepseek.ThinkingEnabled
	}
	req := deepseek.ChatCompletionRequest{
		Model:           model,
		Messages:        messages,
		Thinking:        &deepseek.ThinkingConfig{Type: thinkingType},
		ReasoningEffort: effort,
		MaxTokens:       maxTokens,
		Tools:           tools.Definitions(),
	}

	release, err := r.acquireModelSlot(ctx, model)
	if err != nil {
		return "", "", nil, "", nil, err
	}
	defer release()

	events, err := r.Client.StreamChatCompletion(ctx, req)
	if err != nil {
		return "", "", nil, "", nil, err
	}

	var reasoningBuf, contentBuf strings.Builder
	assembler = deepseek.NewToolCallAssembler()
	var streamErr error
	for ev := range events {
		switch ev.Type {
		case deepseek.EventReasoningDelta:
			reasoningBuf.WriteString(ev.Reasoning)
		case deepseek.EventContentDelta:
			contentBuf.WriteString(ev.Content)
		case deepseek.EventToolCallDelta:
			assembler.Add(ev.ToolCall)
		case deepseek.EventFinish:
			finishReason = ev.FinishReason
		case deepseek.EventUsage:
			usage = ev.Usage
		case deepseek.EventError:
			streamErr = ev.Err
		}
	}
	if streamErr != nil {
		return "", "", nil, "", nil, streamErr
	}
	return reasoningBuf.String(), contentBuf.String(), assembler, finishReason, usage, nil
}
