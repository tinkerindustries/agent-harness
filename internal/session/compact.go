package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mrgeoffrich/deepseek-harness/internal/deepseek"
	"github.com/mrgeoffrich/deepseek-harness/internal/fold"
	"github.com/mrgeoffrich/deepseek-harness/internal/httplog"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

// compact forks a new session seeded with a summary of sess, marks sess
// compacted, and returns the new session and its (short) event log so Run
// can keep going under the new id. Compaction rewrites history, so it must
// be a new session rather than an edit to the old one — that is the only
// way to reset the cache deliberately instead of by accident
// (docs/CACHE.md, docs/TOOLS.md).
func (r *Runner) compact(ctx context.Context, sess store.Session, allEvents []store.Event, workspace string) (store.Session, []store.Event, error) {
	messages, err := fold.Fold(sess, allEvents)
	if err != nil {
		return sess, allEvents, fmt.Errorf("session: fold for compaction: %w", err)
	}
	summary, err := r.summarize(httplog.WithSessionID(ctx, sess.ID), messages)
	if err != nil {
		return sess, allEvents, fmt.Errorf("session: summarise for compaction: %w", err)
	}

	newSess := store.Session{
		ID:              newID("sess"),
		ParentID:        sess.ID,
		JobType:         sess.JobType,
		ParentAgentType: sess.ParentAgentType,
		ParentAgentID:   sess.ParentAgentID,
		Model:           sess.Model,
		Effort:          sess.Effort,
		Thinking:        sess.Thinking,
		Workspace:       sess.Workspace,
		PermissionMode:  sess.PermissionMode,
		DenyPatterns:    sess.DenyPatterns,
		SystemPrompt:    RenderCompactionSummarySystemPrompt(summary),
		ToolSchema:      sess.ToolSchema,
		ResultSchema:    sess.ResultSchema,
		Status:          store.StatusRunning,
	}
	if err := r.Store.CreateSession(ctx, newSess); err != nil {
		return sess, allEvents, fmt.Errorf("session: create compacted session: %w", err)
	}
	canonical, err := r.Store.GetSession(ctx, newSess.ID)
	if err != nil {
		return sess, allEvents, fmt.Errorf("session: reload compacted session: %w", err)
	}
	r.openLog(canonical)
	if r.Mirror != nil {
		if err := r.Mirror.Init(canonical, nil); err != nil {
			return sess, allEvents, fmt.Errorf("session: init mirror for compacted session: %w", err)
		}
	}
	r.publishState(ctx, canonical)

	opening := RenderCompactionOpeningMessage(workspace)
	appended, err := r.Store.AppendEvents(ctx, newSess.ID, []store.EventInput{
		{Kind: store.KindSessionStarted, Payload: store.SessionStartedPayload{OpeningMessage: opening}},
	})
	if err != nil {
		return sess, allEvents, fmt.Errorf("session: record compacted session start: %w", err)
	}
	r.mirrorAppend(canonical, appended)
	r.publishEvents(canonical, appended)

	finished := time.Now().UTC()
	if err := r.Store.UpdateSessionStatus(ctx, sess.ID, store.StatusCompacted, &finished); err != nil {
		return sess, allEvents, fmt.Errorf("session: mark parent compacted: %w", err)
	}
	r.closeLog(sess.ID)
	if updated, err := r.Store.GetSession(ctx, sess.ID); err == nil {
		r.mirrorUpdateSession(updated)
		r.publishState(ctx, updated)
	}

	return canonical, appended, nil
}

// summarize asks flash, thinking disabled, to compress a folded transcript
// for continuation in a new session. Non-thinking side work runs cheaper
// and can force output shape, though a plain instruction is enough here
// (docs/MODELS.md).
func (r *Runner) summarize(ctx context.Context, messages []deepseek.Message) (string, error) {
	var b strings.Builder
	for _, m := range messages {
		if m.Content == "" {
			continue
		}
		fmt.Fprintf(&b, "[%s] %s\n", m.Role, m.Content)
	}

	req := deepseek.ChatCompletionRequest{
		Model: r.flashModel(),
		Messages: []deepseek.Message{
			deepseek.SystemMessage("Summarise the following agent session transcript so the work can continue in a new session without it. " +
				"Cover: the original task, what has been done, the current state of the workspace, and what remains."),
			deepseek.UserMessage(b.String()),
		},
		Thinking:  &deepseek.ThinkingConfig{Type: deepseek.ThinkingDisabled},
		MaxTokens: 8000,
	}
	release, err := r.acquireModelSlot(ctx, req.Model)
	if err != nil {
		return "", err
	}
	defer release()
	resp, err := r.Client.CreateChatCompletion(ctx, req)
	if err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", errors.New("session: compaction summary returned no choices")
	}
	return resp.Choices[0].Message.Content, nil
}
