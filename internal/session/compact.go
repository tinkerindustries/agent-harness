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
	items, err := fold.Fold(sess, allEvents)
	if err != nil {
		return sess, allEvents, fmt.Errorf("session: fold for compaction: %w", err)
	}
	summary, err := r.summarize(httplog.WithSessionID(ctx, sess.ID), items, priorCheckpoint(sess.SystemPrompt))
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

// compactionInstruction is the summariser's system prompt. The section
// headings are what a continuing session reads back from its system prompt.
const compactionInstruction = `Summarise the following agent session transcript so the work can continue in a new session without it. The reader has not seen the transcript and cannot ask you questions.

Write these sections, in this order. Write "None" under a section with nothing to report.

## Intent
What the user asked for, in their words where the wording matters, including the acceptance criteria and every later change to the request.

## User corrections and constraints
Each correction, preference or constraint the user gave, and anything they told the agent not to do. Keep these literal.

## Files
Each file read, created or changed, with a line on what was done to it and why.

## Errors and fixes
Each error hit, what caused it, and how it was resolved or that it is still open. Include approaches that failed so they are not retried.

## Work completed
What is finished and how it was verified, with the exact commands that were run and whether they passed.

## Current work
What was in progress at the end of the transcript, and the state it was left in.

## Pending work
Everything still to do, including work the user asked for that has not started.

## Next action
The single next step, specific enough to start without re-reading anything.

## Critical details
Exact commands, paths, identifiers, branch names, URLs, ports and values the work depends on. Copy them verbatim.

If the transcript starts with a previous checkpoint, merge it into these sections. Keep what still holds, update what has changed, and drop facts that no longer apply. Do not describe the checkpoint as a separate earlier summary.`

// compactionTranscript renders the input to the summariser: the previous
// checkpoint if there is one, then one labelled line per item that carries
// prose or a tool call. A tool call's arguments are cut at
// maxCompactionCallArgs because the commands and paths in them are what the
// summary has to preserve, but a patch or file body is not. A reasoning item
// is left out: it does not survive compaction.
func compactionTranscript(items []wire.Item, checkpoint string) string {
	var b strings.Builder
	if checkpoint != "" {
		fmt.Fprintf(&b, "[previous checkpoint]\n%s\n[end of previous checkpoint]\n\n", checkpoint)
	}
	for _, item := range items {
		switch item.Type {
		case wire.ItemMessage:
			if text := item.Content.String(); text != "" {
				fmt.Fprintf(&b, "[%s] %s\n", item.Role, text)
			}
		case wire.ItemFunctionCall:
			args := item.Arguments
			if len(args) > maxCompactionCallArgs {
				args = args[:maxCompactionCallArgs] + "…(truncated)"
			}
			fmt.Fprintf(&b, "[call] %s %s\n", item.Name, args)
		case wire.ItemFunctionCallOutput:
			if text := item.Output.String(); text != "" {
				fmt.Fprintf(&b, "[tool] %s\n", text)
			}
		}
	}
	return b.String()
}

const maxCompactionCallArgs = 2000

// summarize asks flash, thinking disabled, to compress a folded transcript
// for continuation in a new session. Non-thinking side work runs cheaper
// and can force output shape, though a plain instruction is enough here
// (docs/MODELS.md). The intent states thinking:false and no tools; the
// provider spells that as it spells its own non-thinking mode.
func (r *Runner) summarize(ctx context.Context, items []wire.Item, checkpoint string) (string, error) {
	intent := wire.ChatIntent{
		Model: r.flashModel(ctx),
		Items: []wire.Item{
			wire.SystemItem(compactionInstruction),
			wire.UserItem(compactionTranscript(items, checkpoint)),
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
