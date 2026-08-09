package mcp

import (
	"encoding/json"
	"fmt"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mrgeoffrich/deepseek-harness/internal/queue"
)

// maxResultTextChars bounds how much of queue.Result.Text a tool result
// inlines. A run's answer can be long; the transcript resource holds the
// whole thing, so truncating here loses nothing a caller cannot go read.
const maxResultTextChars = 4000

func transcriptURL(publicBaseURL, sessionID string) string {
	if sessionID == "" {
		return ""
	}
	return strings.TrimRight(publicBaseURL, "/") + "/sessions/" + sessionID
}

func transcriptResourceURI(sessionID string) string {
	return "harness://session/" + sessionID + "/transcript"
}

// errorResult builds an IsError tool result from a formatted message. This
// is the shape for every genuine failure this package reports: a bad
// argument, a repository git would not accept, a NATS publish that failed.
func errorResult(format string, args ...any) *mcpsdk.CallToolResult {
	return &mcpsdk.CallToolResult{
		IsError: true,
		Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: fmt.Sprintf(format, args...)}},
	}
}

// renderFinal builds deepseek_result's tool result for a terminal
// queue.Result: Result.Text (truncated, pointing at the transcript resource
// when it is), Complete's payload in StructuredContent when present, and a
// footer with status, sub-turns, cost, and the browser transcript URL.
// complete_status reads as a give-up rather than folding silently into an
// "ok" the caller has no way to tell from a finished task.
func renderFinal(publicBaseURL string, res queue.Result) *mcpsdk.CallToolResult {
	text, truncated := truncateText(res.Text)

	var b strings.Builder
	if text != "" {
		b.WriteString(text)
		b.WriteString("\n\n")
	}
	b.WriteString("---\n")
	fmt.Fprintf(&b, "status: %s\n", res.Status)
	if res.CompleteStatus != "" {
		fmt.Fprintf(&b, "complete_status: %s\n", res.CompleteStatus)
	}
	fmt.Fprintf(&b, "sub_turns: %d\n", res.SubTurns)
	if res.Usage != nil {
		fmt.Fprintf(&b, "cost: $%.6f USD (price table %s)\n", res.Usage.CostUSD, res.Usage.PriceTableDate)
	}
	if res.Error != nil {
		fmt.Fprintf(&b, "error: %s: %s\n", res.Error.Code, res.Error.Message)
	}
	if url := transcriptURL(publicBaseURL, res.SessionID); url != "" {
		fmt.Fprintf(&b, "transcript: %s\n", url)
	}
	if truncated && res.SessionID != "" {
		fmt.Fprintf(&b, "(text truncated; full transcript at %s)\n", transcriptResourceURI(res.SessionID))
	}

	result := &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: b.String()}}}
	if len(res.Result) > 0 {
		var v any
		if err := json.Unmarshal(res.Result, &v); err == nil {
			result.StructuredContent = v
		}
	}
	return result
}

// renderRunning is deepseek_result's answer when the run has not finished
// but has progressed at least once: sub-turn count and cost so far, from
// the most recent .progress message.
func renderRunning(publicBaseURL, requestID string, p queue.Progress) *mcpsdk.CallToolResult {
	var b strings.Builder
	fmt.Fprintf(&b, "status: running\n")
	fmt.Fprintf(&b, "sub_turn: %d\n", p.SubTurn)
	if p.Usage != nil {
		fmt.Fprintf(&b, "cost so far: $%.6f USD (price table %s)\n", p.Usage.CostUSD, p.Usage.PriceTableDate)
	}
	if p.Churned {
		b.WriteString("cache: churned this sub-turn\n")
	}
	if url := transcriptURL(publicBaseURL, p.SessionID); url != "" {
		fmt.Fprintf(&b, "transcript: %s\n", url)
	}
	return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: b.String()}}}
}

// renderQueued is deepseek_result's answer when neither a final result nor
// a progress message has arrived: normal for a request still waiting on a
// free worker slot (docs/DESIGN.md §4.10), not an error.
func renderQueued(requestID string) *mcpsdk.CallToolResult {
	text := fmt.Sprintf("status: queued\nrequest_id: %s\nno accepted or progress message seen yet; call deepseek_result again later.\n", requestID)
	return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: text}}}
}

func truncateText(s string) (text string, truncated bool) {
	if len(s) <= maxResultTextChars {
		return s, false
	}
	return s[:maxResultTextChars] + "…", true
}
