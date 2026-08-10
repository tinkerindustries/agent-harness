package mcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

// statusInput is deepseek_status's argument set: a request id and nothing
// else. The tool never waits — it reads the harness's read-only status
// endpoint, which answers immediately.
type statusInput struct {
	RequestID string `json:"request_id" jsonschema:"The request_id returned by deepseek_agent."`
}

// statusResponse mirrors GET /api/requests/{request_id}/status's wire shape
// (internal/httpapi). The store payload types are reused as-is, the same way
// this package reuses store.Event for the transcript resource.
type statusResponse struct {
	RequestID     string                 `json:"request_id"`
	SessionID     string                 `json:"session_id"`
	Status        string                 `json:"status"`
	SubTurn       int                    `json:"sub_turn"`
	Todos         []store.StatusTodo     `json:"todos"`
	ActiveForm    string                 `json:"active_form"`
	ToolCalls     []store.StatusToolCall `json:"tool_calls"`
	Usage         *store.UsagePayload    `json:"usage"`
	StartedAt     time.Time              `json:"started_at"`
	DurationMS    int64                  `json:"duration_ms"`
	TranscriptURL string                 `json:"transcript_url"`
	ErrorCode     string                 `json:"error_code"`
	ErrorMessage  string                 `json:"error_message"`
}

func (svc *Service) registerStatusTool(server *mcpsdk.Server) {
	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name: "deepseek_status",
		Description: "Report where a deepseek-harness run is up to, by request_id. Never blocks: it reads the " +
			"harness's read-only status endpoint and returns immediately with the current state — what the run " +
			"is working on, its todo list, tools in flight, and cost so far. Use while a run is in flight, and " +
			"deepseek_result once it has finished.",
	}, svc.handleStatus)
}

func (svc *Service) handleStatus(ctx context.Context, _ *mcpsdk.CallToolRequest, in statusInput) (*mcpsdk.CallToolResult, any, error) {
	if in.RequestID == "" {
		return errorResult("request_id is required"), nil, nil
	}

	var st statusResponse
	if err := getJSON(ctx, svc.HTTPClient, svc.Cfg.HarnessBaseURL, "/api/requests/"+in.RequestID+"/status", &st); err != nil {
		return errorResult("status: %v", err), nil, nil
	}

	result := renderStatus(svc.Cfg.HarnessPublicURL, st)
	return result, st, nil
}

// renderStatus is deepseek_status's answer: a compact text summary of one
// request's current state, enough to answer "how is it going" without
// opening the transcript.
func renderStatus(publicBaseURL string, st statusResponse) *mcpsdk.CallToolResult {
	var b strings.Builder
	fmt.Fprintf(&b, "status: %s\n", st.Status)
	fmt.Fprintf(&b, "request_id: %s\n", st.RequestID)
	if st.SubTurn > 0 {
		fmt.Fprintf(&b, "sub_turn: %d\n", st.SubTurn)
	}
	if st.ActiveForm != "" {
		fmt.Fprintf(&b, "working on: %s\n", st.ActiveForm)
	}
	for _, t := range st.Todos {
		mark := "[ ]"
		switch t.Status {
		case "completed":
			mark = "[x]"
		case "in_progress":
			mark = "[~]"
		}
		fmt.Fprintf(&b, "%s %s\n", mark, t.Content)
	}
	for _, c := range st.ToolCalls {
		fmt.Fprintf(&b, "tool in flight: %s(%s)\n", c.Name, c.Arguments)
	}
	if st.Usage != nil {
		fmt.Fprintf(&b, "cost so far: $%.6f USD\n", st.Usage.CostUSD)
	}
	if st.ErrorCode != "" {
		fmt.Fprintf(&b, "error: %s: %s\n", st.ErrorCode, st.ErrorMessage)
	}
	if url := transcriptURL(publicBaseURL, st.SessionID); url != "" {
		fmt.Fprintf(&b, "transcript: %s\n", url)
	}
	return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: b.String()}}, StructuredContent: st}
}
