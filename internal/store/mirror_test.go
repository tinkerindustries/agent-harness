package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

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
	// store and the live mirror, occasionally rewriting the transcript, the
	// way a real sub-turn boundary would.
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

	var all []Event
	for _, batch := range batches {
		appended, err := s.AppendEvents(ctx, sess.ID, batch)
		if err != nil {
			t.Fatal(err)
		}
		if err := live.AppendEvents(created, appended); err != nil {
			t.Fatalf("live append: %v", err)
		}
		all = append(all, appended...)
		if err := live.WriteTranscript(created, all); err != nil {
			t.Fatalf("live transcript: %v", err)
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
	if err := live.WriteTranscript(finalSess, all); err != nil {
		t.Fatal(err)
	}

	exportRoot := t.TempDir()
	exportMirror := NewMirror(exportRoot)
	if err := ExportTo(ctx, s, exportMirror, sess.ID); err != nil {
		t.Fatalf("export: %v", err)
	}

	liveDir := live.Dir(finalSess)
	exportDir := exportMirror.Dir(finalSess)

	for _, name := range []string{"session.json", "events.jsonl", "transcript.md"} {
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
