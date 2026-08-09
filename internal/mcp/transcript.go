package mcp

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mrgeoffrich/deepseek-harness/internal/hub"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

// renderTranscriptMarkdown renders a session's event log as readable
// markdown for the harness://session/<id>/transcript resource. It is a
// smaller cousin of store.RenderTranscript: that function takes a
// store.Session read straight off the database, which carries fields (like
// Thinking) the harness's read-only HTTP API does not expose; this one
// works from exactly what GET /api/sessions/{id} and .../events actually
// return, reusing store.Event and its payload types to decode them.
func renderTranscriptMarkdown(sess hub.SessionState, events []store.Event) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Session %s\n\n", sess.ID)
	fmt.Fprintf(&b, "- model: %s (effort %s)\n", sess.Model, sess.Effort)
	fmt.Fprintf(&b, "- workspace: %s\n", sess.Workspace)
	fmt.Fprintf(&b, "- permission mode: %s\n", sess.PermissionMode)
	fmt.Fprintf(&b, "- status: %s\n\n", sess.Status)

	sawStart := false
	for _, e := range events {
		switch e.Kind {
		case store.KindSessionStarted:
			var p store.SessionStartedPayload
			_ = json.Unmarshal(e.Payload, &p)
			if !sawStart {
				fmt.Fprintf(&b, "## Task\n\n%s\n\n", p.OpeningMessage)
				sawStart = true
			} else {
				fmt.Fprintf(&b, "## Resumed\n\n%s\n\n", p.OpeningMessage)
			}
		case store.KindTurnStarted:
			var p store.TurnStartedPayload
			_ = json.Unmarshal(e.Payload, &p)
			fmt.Fprintf(&b, "## Sub-turn %d\n\n", p.SubTurn)
		case store.KindContentDelta:
			var p store.ContentDeltaPayload
			_ = json.Unmarshal(e.Payload, &p)
			b.WriteString(p.Text)
		case store.KindToolCall:
			var p store.ToolCallPayload
			_ = json.Unmarshal(e.Payload, &p)
			fmt.Fprintf(&b, "\n\n**tool call** `%s(%s)`\n", p.Name, p.Arguments)
		case store.KindToolResult:
			var p store.ToolResultPayload
			_ = json.Unmarshal(e.Payload, &p)
			label := "tool result"
			if p.IsError {
				label = "tool error"
			}
			fmt.Fprintf(&b, "\n```\n%s [%s]\n%s\n```\n", label, p.Name, p.Content)
		case store.KindToolDenied:
			var p store.ToolDeniedPayload
			_ = json.Unmarshal(e.Payload, &p)
			fmt.Fprintf(&b, "\n**denied** `%s`: %s\n", p.Name, p.Rule)
		case store.KindUsage:
			var p store.UsagePayload
			_ = json.Unmarshal(e.Payload, &p)
			fmt.Fprintf(&b, "\n_usage: prompt %d (hit %d / miss %d), completion %d, cost $%.6f_\n",
				p.PromptTokens, p.PromptCacheHitTokens, p.PromptCacheMissTokens, p.CompletionTokens, p.CostUSD)
		case store.KindTurnFinished:
			b.WriteString("\n\n")
		case store.KindRunFinished:
			var p store.RunFinishedPayload
			_ = json.Unmarshal(e.Payload, &p)
			fmt.Fprintf(&b, "\n## Run finished (%s)\n\n%s\n", p.Reason, p.Summary)
			if p.Status != "" {
				fmt.Fprintf(&b, "\ncomplete status: %s\n", p.Status)
			}
		case store.KindError:
			var p store.ErrorPayload
			_ = json.Unmarshal(e.Payload, &p)
			fmt.Fprintf(&b, "\n## Error\n\n%s\n", p.Message)
		}
	}
	return b.String()
}
