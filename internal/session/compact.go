package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/fold"
	"github.com/mrgeoffrich/agent-harness/internal/httplog"
	"github.com/mrgeoffrich/agent-harness/internal/store"
	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// compact forks a new session seeded with a summary of sess, marks sess
// compacted, and returns the new session and its (short) event log so Run
// can keep going under the new id. Compaction rewrites history, so it must
// be a new session rather than an edit to the old one — that is the only
// way to reset the cache deliberately instead of by accident
// (docs/CACHE.md, docs/TOOLS.md).
//
// The new row is a copy of sess, not built from RunOptions.session. That
// was tried first — opts is the RunOptions the enclosing Run call carries,
// and for a run that reached this point through Runner.Run its fields do
// agree with sess's — but Runner.Resume builds its own RunOptions from the
// session row and never sets Title, Description, Phase, TotalPhases, or
// Prompt (resume.go), because a resumed session's prefix is frozen and none
// of those is a caller's to choose again. A compacted successor of a
// resumed run therefore got those five fields blank. sess itself carries
// the correct value for every field that should survive compaction — a
// struct copy cannot drop one by omission the way a hand-written field list
// (or a builder fed the wrong source) can — so it is the source of truth
// here, and only Run and Create still use RunOptions.session.
//
// Copying sess wholesale also copies run-scoped state that must not carry
// into a fresh row, so each such field is reset explicitly below: ID and
// ParentID because the successor is a new row whose parent is the session
// it replaces, not that session's own parent; SystemPrompt because the
// successor's is the compaction summary; Status because compaction always
// starts the successor running; CreatedAt because CreateSession treats a
// non-zero value as "use this timestamp" (store/sessions.go) — left alone,
// the copy would silently backdate the successor to when the parent was
// created; FinishedAt, Version, CompleteStatus, Plan, RecentToolCalls, and
// Summary because they are the parent's own terminal/live-run bookkeeping,
// not something a session that has not run a single sub-turn yet should
// carry (CreateSession's INSERT does not read Version or FinishedAt at
// all — it hardcodes version 1 and finished_at NULL — but the Go value is
// reset too, so nothing here suggests otherwise to a future reader).
// ToolSchema and ResultSchema are deliberately left as the copy gives them:
// the compacted session keeps the same tool array and result schema the
// run it continues was already using.
func (r *Runner) compact(ctx context.Context, sess store.Session, allEvents []store.Event, workspace string) (store.Session, []store.Event, error) {
	messages, err := fold.Fold(sess, allEvents)
	if err != nil {
		return sess, allEvents, fmt.Errorf("session: fold for compaction: %w", err)
	}
	summary, err := r.summarize(httplog.WithSessionID(ctx, sess.ID), messages)
	if err != nil {
		return sess, allEvents, fmt.Errorf("session: summarise for compaction: %w", err)
	}

	newSess := sess
	newSess.ID = newID("sess")
	newSess.ParentID = sess.ID
	newSess.SystemPrompt = RenderCompactionSummarySystemPromptFor(sess.Model, sess.PromptVariant, summary)
	newSess.Status = store.StatusRunning
	newSess.CreatedAt = time.Time{}
	newSess.FinishedAt = nil
	newSess.Version = 0
	newSess.CompleteStatus = ""
	newSess.Plan = ""
	newSess.RecentToolCalls = nil
	newSess.Summary = ""
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
// (docs/MODELS.md). The intent states thinking:false and no tools; the
// provider spells that as it spells its own non-thinking mode.
func (r *Runner) summarize(ctx context.Context, messages []wire.Message) (string, error) {
	var b strings.Builder
	for _, m := range messages {
		if m.Content.String() == "" {
			continue
		}
		fmt.Fprintf(&b, "[%s] %s\n", m.Role, m.Content.String())
	}

	intent := wire.ChatIntent{
		Model: r.flashModel(ctx),
		Messages: []wire.Message{
			wire.SystemMessage("Summarise the following agent session transcript so the work can continue in a new session without it. " +
				"Cover: the original task, what has been done, the current state of the workspace, and what remains."),
			wire.UserMessage(b.String()),
		},
		Thinking:  false,
		MaxTokens: 8000,
	}
	release, err := r.acquireModelSlot(ctx, intent.Model)
	if err != nil {
		return "", err
	}
	defer release()
	resp, err := r.clientFor(intent.Model).CreateChatCompletion(ctx, intent)
	if err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", errors.New("session: compaction summary returned no choices")
	}
	return resp.Choices[0].Message.Content.String(), nil
}
