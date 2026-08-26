package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/agentmeta"
)

// TestSessionJSONProvenance pins how the provenance fields appear in
// session.json: job_type is always present, and the parent agent fields are
// omitted when empty.
func TestSessionJSONProvenance(t *testing.T) {
	base := Session{
		ID:             "sess-1",
		Model:          "deepseek-v4-pro",
		Effort:         "high",
		Workspace:      "/tmp/ws",
		PermissionMode: "default",
		SystemPrompt:   "sys",
		ToolSchema:     json.RawMessage(`[]`),
		Status:         "running",
		CreatedAt:      time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}

	populated := base
	populated.JobType = agentmeta.JobTypeOrchestration
	populated.ParentAgentType = "orchestrator"
	populated.ParentAgentID = "orchestrator-1"
	populated.ParentIsUser = true
	populated.CompleteStatus = "gave_up"
	populated.Task = "carry the job's description onto the session row"
	populated.Title = "Add session title fields"
	populated.Description = "Carry a title, description, and phase position from every producer onto the session row and the main page."
	populated.Phase = 2
	populated.TotalPhases = 5
	populated.Plan = `[{"content":"a","status":"completed","activeForm":""}]`
	populated.RecentToolCalls = []RecentToolCall{{Name: "Bash", Arguments: `{"command":"go build"}`}}
	populated.Summary = "wired it up"
	b, err := json.Marshal(toSessionJSON(populated))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"job_type":"orchestration"`, `"parent_agent_type":"orchestrator"`, `"parent_agent_id":"orchestrator-1"`, `"parent_is_user":true`, `"complete_status":"gave_up"`, `"task":"carry the job`, `"title":"Add session title fields"`, `"description":"Carry a title`, `"phase":2`, `"total_phases":5`, `"plan":[{"content":"a"`, `"recent_tool_calls":[{"name":"Bash"`, `"summary":"wired it up"`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("session.json missing %s: %s", want, b)
		}
	}

	empty := base
	b, err = json.Marshal(toSessionJSON(empty))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"job_type":""`) {
		t.Fatalf("job_type should always be written, got: %s", b)
	}
	for _, absent := range []string{"parent_agent_type", "parent_agent_id", "parent_is_user", "complete_status", "task", "title", "description", "phase", "total_phases", "plan", "recent_tool_calls", "summary"} {
		if strings.Contains(string(b), absent) {
			t.Fatalf("expected %s omitted when empty, got: %s", absent, b)
		}
	}
}

// TestExportMatchesLiveMirror simulates a live run writing its mirror
// incrementally, then rebuilds the same session into a separate directory
// with ExportTo, and asserts the two directories are byte-identical. Export
// is the crash-repair path; this is what proves it actually reproduces what
// a live run would have written (docs/DESIGN.md §4.8).
func TestExportMatchesLiveMirror(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	sess := Session{
		ID:             "sess-live",
		Model:          "deepseek-v4-pro",
		Effort:         "high",
		Thinking:       true,
		Workspace:      "/tmp/ws",
		PermissionMode: "default",
		DenyPatterns:   []string{},
		SystemPrompt:   "you are an agent",
		ToolSchema:     json.RawMessage(`[{"type":"function","function":{"name":"Read"}}]`),
	}
	if err := s.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	created, err := s.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}

	liveRoot := t.TempDir()
	live := NewMirror(liveRoot)
	if err := live.Init(created, nil); err != nil {
		t.Fatalf("live init: %v", err)
	}

	// Simulate the runner: append a batch of events, write them to both the
	// store and the live mirror, the way a real sub-turn boundary would.
	batches := [][]EventInput{
		{
			{Kind: KindSessionStarted, Payload: SessionStartedPayload{OpeningMessage: "fix the bug"}},
			{Kind: KindTurnStarted, Payload: TurnStartedPayload{SubTurn: 1}},
			{Kind: KindReasoningDelta, Payload: ReasoningDeltaPayload{Text: "thinking"}},
			{Kind: KindToolCall, Payload: ToolCallPayload{Index: 0, ID: "call_00_a", Name: "Read", Arguments: `{"file_path":"a.go"}`}},
			{Kind: KindTurnFinished, Payload: TurnFinishedPayload{FinishReason: "tool_calls"}},
		},
		{
			{Kind: KindUsage, Payload: UsagePayload{PromptTokens: 500, PromptCacheHitTokens: 400, PromptCacheMissTokens: 100}},
			{Kind: KindToolResult, Payload: ToolResultPayload{ToolCallID: "call_00_a", Name: "Read", Content: "1\tpackage a"}},
		},
		{
			{Kind: KindTurnStarted, Payload: TurnStartedPayload{SubTurn: 2}},
			{Kind: KindContentDelta, Payload: ContentDeltaPayload{Text: "fixed."}},
			{Kind: KindTurnFinished, Payload: TurnFinishedPayload{FinishReason: "stop"}},
			{Kind: KindRunFinished, Payload: RunFinishedPayload{Reason: "no_tool_calls", Text: "fixed."}},
		},
	}

	for _, batch := range batches {
		appended, err := s.AppendEvents(ctx, sess.ID, batch)
		if err != nil {
			t.Fatal(err)
		}
		if err := live.AppendEvents(created, appended); err != nil {
			t.Fatalf("live append: %v", err)
		}
	}

	finalSess, err := s.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	finalSess.Status = StatusOK
	if err := s.UpdateSessionStatus(ctx, sess.ID, StatusOK, nil); err != nil {
		t.Fatal(err)
	}
	finalSess, err = s.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := live.UpdateSession(finalSess); err != nil {
		t.Fatal(err)
	}

	exportRoot := t.TempDir()
	exportMirror := NewMirror(exportRoot)
	if err := ExportTo(ctx, s, exportMirror, sess.ID); err != nil {
		t.Fatalf("export: %v", err)
	}

	liveDir := live.Dir(finalSess)
	exportDir := exportMirror.Dir(finalSess)

	for _, name := range []string{"session.json", "events.jsonl"} {
		liveBytes, err := os.ReadFile(filepath.Join(liveDir, name))
		if err != nil {
			t.Fatalf("read live %s: %v", name, err)
		}
		exportBytes, err := os.ReadFile(filepath.Join(exportDir, name))
		if err != nil {
			t.Fatalf("read export %s: %v", name, err)
		}
		if string(liveBytes) != string(exportBytes) {
			t.Fatalf("%s differs between live mirror and export:\n--- live ---\n%s\n--- export ---\n%s", name, liveBytes, exportBytes)
		}
	}
}
