package session

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mrgeoffrich/deepseek-harness/internal/cache"
	"github.com/mrgeoffrich/deepseek-harness/internal/fold"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
	"github.com/mrgeoffrich/deepseek-harness/internal/tools"
)

// ResumeOptions is what Resume needs beyond the session it is continuing.
// Model, effort, thinking, workspace, permission mode, deny patterns, and
// result schema all come from the frozen session row instead — a resumed
// session's prefix cannot change (docs/CACHE.md), so none of those are a
// caller's to choose again.
type ResumeOptions struct {
	SessionID string
	// Prompt, when non-empty, is appended as a new user message before the
	// loop continues. Left empty, Resume just re-runs sub-turns against the
	// session's existing log — the shape a run that stopped at
	// MaxSubTurns without finishing needs.
	Prompt      string
	MaxTokens   int
	MaxSubTurns int
	Resolver    tools.Resolver
	Progress    func(SubTurnProgress)
}

// Resume continues a session that reached a terminal status. It refuses a
// session that is still running (a live goroutine already owns it) or that
// was retired by compaction (its continuation is the compacted child, found
// at the child's ParentID, not this session).
func (r *Runner) Resume(ctx context.Context, opts ResumeOptions) (*RunResult, error) {
	sess, err := r.Store.GetSession(ctx, opts.SessionID)
	if err != nil {
		return nil, fmt.Errorf("session: resume: load %s: %w", opts.SessionID, err)
	}
	switch sess.Status {
	case store.StatusRunning:
		return nil, fmt.Errorf("session: resume: %s is still running", opts.SessionID)
	case store.StatusCompacted:
		return nil, fmt.Errorf("session: resume: %s was retired by compaction; resume its child session instead", opts.SessionID)
	}

	allEvents, err := r.Store.GetEvents(ctx, sess.ID)
	if err != nil {
		return nil, fmt.Errorf("session: resume: load events: %w", err)
	}
	detector := primeDetector(sess, allEvents)
	startSubTurn := countTurns(allEvents) + 1

	policy := &tools.Policy{
		Mode:     tools.Mode(sess.PermissionMode),
		Deny:     sess.DenyPatterns,
		Resolver: opts.Resolver,
	}
	executor, err := tools.NewExecutor(sess.Workspace, policy)
	if err != nil {
		return nil, err
	}
	executor.Client = r.clientFor(r.flashModel(ctx))
	executor.Prices = r.Prices
	executor.FlashModel = r.flashModel(ctx)
	executor.Gemini = r.Gemini
	executor.GeminiModel = r.GeminiModel
	executor.Settings = r.Settings
	executor.ResultSchema = sess.ResultSchema

	runOpts := RunOptions{
		Model: sess.Model, Effort: sess.Effort, Thinking: sess.Thinking,
		MaxTokens: opts.MaxTokens, Workspace: sess.Workspace,
		PermissionMode: tools.Mode(sess.PermissionMode), Deny: sess.DenyPatterns,
		ResultSchema: sess.ResultSchema, MaxSubTurns: opts.MaxSubTurns,
		Resolver: opts.Resolver, ParentID: sess.ParentID,
		JobType: sess.JobType, ParentAgentType: sess.ParentAgentType, ParentAgentID: sess.ParentAgentID,
		ParentIsUser: sess.ParentIsUser,
		SessionID:    sess.ID, Progress: opts.Progress,
	}
	executor.RunSubagent = r.subagentRunner(sess.ID, runOpts, executor.Workspace)

	if opts.Prompt != "" {
		appended, err := r.Store.AppendEvents(ctx, sess.ID, []store.EventInput{
			{Kind: store.KindSessionStarted, Payload: store.SessionStartedPayload{OpeningMessage: opts.Prompt}},
		})
		if err != nil {
			return nil, fmt.Errorf("session: resume: record continuation: %w", err)
		}
		r.mirrorAppend(sess, appended)
		r.publishEvents(sess, appended)
		allEvents = append(allEvents, appended...)
	}

	if err := r.Store.ResumeSession(ctx, sess.ID); err != nil {
		return nil, fmt.Errorf("session: resume: mark running: %w", err)
	}
	curSess, err := r.Store.GetSession(ctx, sess.ID)
	if err != nil {
		return nil, fmt.Errorf("session: resume: reload: %w", err)
	}
	r.mirrorUpdateSession(curSess)
	r.publishState(ctx, curSess)

	r.openLog(curSess)
	return r.runLoop(ctx, curSess, allEvents, runOpts, executor, detector, startSubTurn)
}

// countTurns reports how many sub-turns a session's event log already
// carries, so Resume's own numbering continues instead of restarting — the
// session-list and browser both read turn_started counts cumulatively
// across the whole log regardless of a resume boundary
// (store.SessionUsageSummaries), and RunResult.SubTurns matches that.
func countTurns(events []store.Event) int {
	n := 0
	for _, e := range events {
		if e.Kind == store.KindTurnStarted {
			n++
		}
	}
	return n
}

// primeDetector reconstructs the cache.Detector state Observe would have
// left behind at the end of sess's last completed sub-turn, from events
// alone. Without this, Resume would start from a fresh Detector, whose first
// Observe call never reports churn regardless of what actually happened —
// safe, but it silently skips the one check a resume boundary most needs
// (docs/CACHE.md). Any decode failure falls back to a fresh Detector rather
// than failing the resume outright.
func primeDetector(sess store.Session, events []store.Event) *cache.Detector {
	lastTurnIdx := -1
	for i, e := range events {
		if e.Kind == store.KindTurnStarted {
			lastTurnIdx = i
		}
	}
	if lastTurnIdx < 0 {
		return cache.NewDetector()
	}

	primeMessages, err := fold.Fold(sess, events[:lastTurnIdx])
	if err != nil {
		return cache.NewDetector()
	}
	// The last usage event of the turn, not the first: a sub-turn that hit
	// the reasoning-starved retry commits one per request, and it is the
	// final attempt whose prefix the resumed session builds on.
	var last *store.UsagePayload
	for _, e := range events[lastTurnIdx:] {
		if e.Kind != store.KindUsage {
			continue
		}
		var p store.UsagePayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return cache.NewDetector()
		}
		last = &p
	}
	if last == nil {
		return cache.NewDetector()
	}
	return cache.NewDetectorFrom(last.PromptTokens+last.CompletionTokens, primeMessages)
}
