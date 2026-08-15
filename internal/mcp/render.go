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
// argument, a repository git would not accept, a queue publish that failed.
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

// renderPending is deepseek_result's answer when no final result exists at
// the moment the call is made: the run may still be queued, mid-run, or
// failed before its session existed, and deepseek_status distinguishes them.
// Never an error — a run in flight is the normal case for this tool.
func renderPending(requestID string) *mcpsdk.CallToolResult {
	text := fmt.Sprintf("request_id: %s\nno final result yet; the run is still going. Call deepseek_status with this request_id for where it is up to.\n", requestID)
	return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: text}}}
}

func truncateText(s string) (text string, truncated bool) {
	if len(s) <= maxResultTextChars {
		return s, false
	}
	return s[:maxResultTextChars] + "…", true
}
