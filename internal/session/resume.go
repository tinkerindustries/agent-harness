package session

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/mrgeoffrich/agent-harness/internal/cache"
	"github.com/mrgeoffrich/agent-harness/internal/fold"
	"github.com/mrgeoffrich/agent-harness/internal/store"
	"github.com/mrgeoffrich/agent-harness/internal/tools"
	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// ResumeOptions is what Resume needs beyond the session it is continuing.
// Model, effort, thinking, the prompt variant, workspace, permission mode,
// deny patterns, and result schema all come from the frozen session row
// instead — a resumed session's prefix cannot change (docs/CACHE.md), so none
// of those are a caller's to choose again.
type ResumeOptions struct {
	SessionID string
	// Prompt, when non-empty, is appended as a new user message before the
	// loop continues. Left empty, Resume just re-runs sub-turns against the
	// session's existing log — the shape a run that stopped at
	// MaxSubTurns without finishing needs.
	Prompt string
	// AttachmentIDs names the images the continuation carries, as rows of
	// the attachments table (docs/RUN-CONTROL.md, "Images in the composer").
	// A resumed session keeps the workspace it already has, so nothing
	// prepares one for it — Resume materialises them into that existing
	// workspace itself, before the message naming them is appended.
	AttachmentIDs []string
	MaxTokens     int
	MaxSubTurns   int
}

// Resume continues a session that reached a terminal status. It refuses a
// session that is still live — "running" (a live goroutine already owns it)
// or "creating" (its workspace is still being prepared, so there is no loop
// to continue) — or that was retired by compaction (its continuation is the
// compacted child, found at the child's ParentID, not this session).
func (r *Runner) Resume(ctx context.Context, opts ResumeOptions) (*RunResult, error) {
	sess, err := r.Store.GetSession(ctx, opts.SessionID)
	if err != nil {
		return nil, fmt.Errorf("session: resume: load %s: %w", opts.SessionID, err)
	}
	switch sess.Status {
	case store.StatusRunning:
		return nil, fmt.Errorf("session: resume: %s is still running", opts.SessionID)
	case store.StatusCreating:
		return nil, fmt.Errorf("session: resume: %s is still being prepared", opts.SessionID)
	case store.StatusCompacted:
		return nil, fmt.Errorf("session: resume: %s was retired by compaction; resume its child session instead", opts.SessionID)
	}

	allEvents, err := r.Store.GetEvents(ctx, sess.ID)
	if err != nil {
		return nil, fmt.Errorf("session: resume: load events: %w", err)
	}
	detector := primeDetector(sess, allEvents)
	startSubTurn := countTurns(allEvents) + 1

	// The tool array comes from the session's own stored tool_schema, not a
	// fresh resolution: a server enabled or disabled since the run started
	// must not change what a resumed session sends (docs/MCP.md,
	// "Resolution happens once per run"). An unmarshal failure — a row from
	// before tool_schema existed, or corrupt JSON — falls back to resolving
	// fresh, logged, rather than failing the resume outright.
	toolArray, err := unmarshalToolSchema(sess.ToolSchema)
	mcpDefs, mcpReadOnly := r.resolveMCPDefinitions(ctx, sess.ID)
	if err != nil {
		log.Printf("session: resume: unmarshal stored tool schema for %s, resolving fresh: %v", sess.ID, err)
		toolArray = tools.WithMCP(tools.DefinitionsForVariant(sess.Model, sess.PromptVariant), mcpDefs)
	}

	// The read-only map is not the frozen array: it is the policy a call is
	// checked against, not prefix bytes in the head, so it comes from a
	// fresh call to the provider regardless of which branch the array took
	// — which is what lets an operator revoke a server's allowance and have
	// the next resume honour it. The session's own allowance at run start is
	// on the row (Session.MCPReadOnly) for a caller that has to hold a
	// resume to it; internal/responsesstdio is the one that does, because there
	// the allowance is supplied by the client rather than by an operator.
	policy := &tools.Policy{
		Mode:               tools.Mode(sess.PermissionMode),
		Deny:               sess.DenyPatterns,
		MCPReadOnlyServers: mcpReadOnly,
	}
	executor, err := tools.NewExecutor(sess.Workspace, policy)
	if err != nil {
		return nil, err
	}
	executor.Client = r.clientFor(r.flashModel(ctx))
	executor.Prices = r.Prices
	executor.FlashModel = r.flashModel(ctx)
	executor.SeeImages = seesImages(sess.Model)
	executor.Gemini = r.Gemini
	executor.GeminiModel = r.GeminiModel
	executor.Settings = r.Settings
	executor.ExtraEnv = r.ToolEnv
	executor.EnvFilter = r.ToolEnvFilter
	executor.ResultSchema = sess.ResultSchema
	executor.MCP = r.MCP

	runOpts := RunOptions{
		Model: sess.Model, Effort: sess.Effort, Thinking: sess.Thinking,
		PromptVariant: sess.PromptVariant,
		MaxTokens:     opts.MaxTokens, Workspace: sess.Workspace,
		PermissionMode: tools.Mode(sess.PermissionMode), Deny: sess.DenyPatterns,
		ResultSchema: sess.ResultSchema, MaxSubTurns: opts.MaxSubTurns,
		ParentID: sess.ParentID,
		JobType:  sess.JobType, ParentAgentType: sess.ParentAgentType, ParentAgentID: sess.ParentAgentID,
		ParentIsUser: sess.ParentIsUser,
		SessionID:    sess.ID,
		Tools:        toolArray,
	}
	executor.RunSubagent = r.subagentRunner(sess.ID, runOpts, executor.Workspace)

	// The continuation's images land in the workspace before the message
	// naming them is appended, so the model never reads a path that is not
	// there yet. Unlike a steer's, this failure is fatal: a resume is one
	// message and nothing has happened yet, so refusing it leaves the
	// session exactly as it was and the browser shows the 500 against the
	// message still in the box — whereas a steer's failure arrives mid-run,
	// where ending the run would cost work already done.
	attachmentNames, err := r.materialiseAttachments(ctx, sess.Workspace, opts.AttachmentIDs)
	if err != nil {
		return nil, fmt.Errorf("session: resume: %w", err)
	}

	if opts.Prompt != "" || len(attachmentNames) > 0 {
		// The message the model reads is the attachment block and then the
		// person's words, exactly the order the opening message puts them
		// in. Task keeps the words alone, which is what the transcript
		// renders — the block is addressed to the model, and showing it back
		// to the person who just pasted the images would be repeating the
		// file names at them (web/src/api/fold.ts, the continuation block).
		message := RenderAttachmentBlock("message", attachmentNames) + opts.Prompt
		appended, err := r.Store.AppendEvents(ctx, sess.ID, []store.EventInput{
			{Kind: store.KindSessionStarted, Payload: store.SessionStartedPayload{
				OpeningMessage: message,
				Task:           opts.Prompt,
				Attachments:    attachmentPaths(attachmentNames),
			}},
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

// unmarshalToolSchema decodes sess.ToolSchema back into the array Resume
// sends, so a resumed session's requests match the row it already promised
// (docs/CACHE.md). A nil or empty payload is not valid JSON and is reported
// as an error like any other decode failure, rather than silently resolving
// to an empty array — the caller's fallback path is what handles it.
func unmarshalToolSchema(raw json.RawMessage) ([]wire.Tool, error) {
	var toolArray []wire.Tool
	if err := json.Unmarshal(raw, &toolArray); err != nil {
		return nil, err
	}
	// An empty array is treated as a decode failure rather than as an
	// answer, so the caller resolves fresh. Both shapes that produce one —
	// a stored "null" (a nil slice marshalled by some earlier version) and
	// a literal "[]" — unmarshal without error, and a resumed session that
	// took them at their word would send no tools at all: the model would
	// go quiet with nothing in the log saying why, which reads as a model
	// failure rather than the data problem it is. No live run can store
	// either shape (Run always marshals a non-empty array), so this costs
	// nothing and closes the one silent way to lose every tool.
	if len(toolArray) == 0 {
		return nil, fmt.Errorf("stored tool schema decoded to no tools")
	}
	return toolArray, nil
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
