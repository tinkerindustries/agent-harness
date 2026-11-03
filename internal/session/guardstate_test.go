package session

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/guard"
	"github.com/mrgeoffrich/agent-harness/internal/pricing"
	"github.com/mrgeoffrich/agent-harness/internal/store"
	"github.com/mrgeoffrich/agent-harness/internal/tools"
	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

type logBuilder struct {
	t      *testing.T
	events []store.Event
}

func (b *logBuilder) add(kind store.EventKind, payload any) {
	b.t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		b.t.Fatal(err)
	}
	b.events = append(b.events, store.Event{Seq: int64(len(b.events) + 1), Kind: kind, Payload: raw})
}

func (b *logBuilder) call(id, name string, args any) store.ToolCallPayload {
	b.t.Helper()
	raw, _ := json.Marshal(args)
	p := store.ToolCallPayload{ID: id, Name: name, Arguments: string(raw)}
	b.add(store.KindToolCall, p)
	return p
}

func (b *logBuilder) result(id, content string, isError bool) {
	b.add(store.KindToolResult, store.ToolResultPayload{ToolCallID: id, Content: content, IsError: isError})
}

func (b *logBuilder) bash(id, command string) store.ToolCallPayload {
	p := b.call(id, "bash", map[string]string{"command": command})
	b.result(id, "ok", false)
	return p
}

func guardBuilder(t *testing.T, now time.Time) guard.StateBuilder {
	return guard.StateBuilder{
		FS:     guard.OSFileSystem{},
		Branch: func(context.Context, string) string { return "topic" },
	}
}

func writeFile(t *testing.T, path, text string, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func TestGuardInputsRecentCommandWindow(t *testing.T) {
	ws := t.TempDir()
	b := &logBuilder{t: t}
	b.bash("c1", "echo one")
	b.bash("c2", "echo two")
	b.bash("c3", "echo three")
	b.bash("c4", strings.Repeat("x", 250))
	cur := b.call("c5", "bash", map[string]string{"command": "echo now"})
	later := b.call("c6", "bash", map[string]string{"command": "echo later in the round"})

	in := GuardInputs(ws, GuardHistory{Events: b.events}, GuardCall{
		ID: "c5", Command: "echo now", Round: []store.ToolCallPayload{cur, later},
	})
	if len(in.Commands) != 4 {
		t.Fatalf("Commands = %q, want the four earlier commands", in.Commands)
	}
	st := guard.StateBuilder{Branch: func(context.Context, string) string { return "" }}.Build(t.Context(), in)
	want := []string{"echo two", "echo three", strings.Repeat("x", 200) + "…"}
	if strings.Join(st.RecentCommands, "\n") != strings.Join(want, "\n") {
		t.Fatalf("RecentCommands = %q, want %q", st.RecentCommands, want)
	}
}

func TestGuardInputsLeavesOutDeniedBash(t *testing.T) {
	b := &logBuilder{t: t}
	b.call("c1", "bash", map[string]string{"command": "rm -rf /"})
	b.add(store.KindToolDenied, store.ToolDeniedPayload{ToolCallID: "c1", Name: "bash", Rule: "deny"})
	b.bash("c2", "echo ran")
	in := GuardInputs(t.TempDir(), GuardHistory{Events: b.events}, GuardCall{ID: "c3", Command: "x"})
	if len(in.Commands) != 1 || in.Commands[0] != "echo ran" {
		t.Fatalf("Commands = %q, want only the command that ran", in.Commands)
	}
}

func TestGuardStateOrigins(t *testing.T) {
	ws := t.TempDir()
	start := time.Now().Add(-time.Hour)
	old := start.Add(-24 * time.Hour)
	writeFile(t, filepath.Join(ws, "existing.sh"), "echo existing\n", old)
	writeFile(t, filepath.Join(ws, "edited.sh"), "echo edited\n", start.Add(time.Minute))
	writeFile(t, filepath.Join(ws, "made.sh"), "echo made\n", start.Add(time.Minute))
	writeFile(t, filepath.Join(ws, "shell.sh"), "echo shell\n", start.Add(2*time.Minute))
	writeFile(t, filepath.Join(ws, "failed.sh"), "echo failed\n", old)

	b := &logBuilder{t: t}
	b.call("w1", "write", map[string]string{"file_path": "made.sh", "content": "echo made\n"})
	b.result("w1", "Created made.sh", false)
	b.call("e1", "edit", map[string]string{"file_path": "edited.sh", "old_string": "a", "new_string": "b"})
	b.result("e1", "Edited edited.sh", false)
	b.call("w2", "write", map[string]string{"file_path": "failed.sh", "content": "echo failed\n"})
	b.result("w2", "write failed.sh: disk full", true)
	cur := b.call("b1", "bash", map[string]string{"command": "bash existing.sh; bash edited.sh; bash made.sh; bash shell.sh; bash failed.sh"})

	st := BuildGuardState(t.Context(), guardBuilder(t, start), ws, GuardHistory{Events: b.events, Start: start},
		GuardCall{ID: "b1", Command: "bash existing.sh; bash edited.sh; bash made.sh; bash shell.sh; bash failed.sh", Round: []store.ToolCallPayload{cur}})

	got := map[string]guard.ScriptEntry{}
	for _, e := range st.ScriptsRun {
		got[e.Path] = e
	}
	want := map[string]struct {
		origin  string
		content string
	}{
		"existing.sh": {guard.OriginExisting, ""},
		"edited.sh":   {guard.OriginChangedThisSession, "echo edited\n"},
		"made.sh":     {guard.OriginWrittenThisSession, "echo made\n"},
		"shell.sh":    {guard.OriginChangedThisSession, "echo shell\n"},
		"failed.sh":   {guard.OriginExisting, ""},
	}
	for path, w := range want {
		e, ok := got[path]
		if !ok {
			t.Errorf("%s missing from scripts_run", path)
			continue
		}
		if e.Origin != w.origin {
			t.Errorf("%s origin = %q, want %q", path, e.Origin, w.origin)
		}
		if w.content == "" && e.Content != nil {
			t.Errorf("%s has content %q, want null", path, *e.Content)
		}
		if w.content != "" && (e.Content == nil || *e.Content != w.content) {
			t.Errorf("%s content = %v, want %q", path, e.Content, w.content)
		}
	}
	if st.Branch != "topic" || st.WorkingDirectory != ws {
		t.Errorf("branch and directory wrong: %+v", st)
	}
}

func TestGuardStateTwoCallsInOneRound(t *testing.T) {
	ws := t.TempDir()
	start := time.Now().Add(-time.Hour)
	b := &logBuilder{t: t}
	write := b.call("w1", "write", map[string]string{"file_path": "setup.sh", "content": "echo setup\n"})
	run := b.call("b1", "bash", map[string]string{"command": "bash setup.sh"})

	// The write has not landed, and the round's results are not committed.
	st := BuildGuardState(t.Context(), guardBuilder(t, start), ws, GuardHistory{Events: b.events, Start: start},
		GuardCall{ID: "b1", Command: "bash setup.sh", Round: []store.ToolCallPayload{write, run}})
	if len(st.ScriptsRun) != 1 {
		t.Fatalf("scripts_run = %+v", st.ScriptsRun)
	}
	e := st.ScriptsRun[0]
	if e.Origin != guard.OriginWrittenThisSession || e.Content != nil {
		t.Fatalf("got %+v, want a session script with no content", e)
	}
	if reasons := guard.NewScriptDetector().EscalationReasons("bash setup.sh", st.ScriptsRun); len(reasons) == 0 {
		t.Fatal("a script written in the same round did not escalate")
	}

	// The same write a round earlier, with its result, is readable.
	writeFile(t, filepath.Join(ws, "setup.sh"), "echo setup\n", time.Now())
	b2 := &logBuilder{t: t}
	b2.call("w1", "write", map[string]string{"file_path": "setup.sh", "content": "echo setup\n"})
	b2.result("w1", "Created setup.sh", false)
	next := b2.call("b2", "bash", map[string]string{"command": "bash setup.sh"})
	st = BuildGuardState(t.Context(), guardBuilder(t, start), ws, GuardHistory{Events: b2.events, Start: start},
		GuardCall{ID: "b2", Command: "bash setup.sh", Round: []store.ToolCallPayload{next}})
	if e := st.ScriptsRun[0]; e.Origin != guard.OriginWrittenThisSession || e.Content == nil || *e.Content != "echo setup\n" {
		t.Fatalf("a settled write: got %+v", e)
	}
}

func TestGuardStateApplyPatch(t *testing.T) {
	ws := t.TempDir()
	start := time.Now().Add(-time.Hour)
	writeFile(t, filepath.Join(ws, "added.sh"), "echo added\n", time.Now())
	writeFile(t, filepath.Join(ws, "updated.sh"), "echo updated\n", time.Now())
	patch := "*** Begin Patch\n*** Add File: added.sh\n+echo added\n*** Update File: updated.sh\n@@\n-echo old\n+echo updated\n*** End Patch"
	b := &logBuilder{t: t}
	b.call("p1", "apply_patch", map[string]string{"input": patch})
	b.result("p1", "Applied a patch to 2 files", false)
	cur := b.call("b1", "bash", map[string]string{"command": "bash added.sh; bash updated.sh"})
	st := BuildGuardState(t.Context(), guardBuilder(t, start), ws, GuardHistory{Events: b.events, Start: start},
		GuardCall{ID: "b1", Command: "bash added.sh; bash updated.sh", Round: []store.ToolCallPayload{cur}})
	origins := map[string]string{}
	for _, e := range st.ScriptsRun {
		origins[e.Path] = e.Origin
	}
	if origins["added.sh"] != guard.OriginWrittenThisSession || origins["updated.sh"] != guard.OriginChangedThisSession {
		t.Fatalf("origins = %v", origins)
	}
}

// TestGuardHistoryAcrossCompaction drives a real compaction. The successor's
// own log holds only its opening message, so the write and the command from
// before the fork are found by following the parent link.
func TestGuardHistoryAcrossCompaction(t *testing.T) {
	var streams int32Counter
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &probe)
		if !probe.Stream {
			resp := wire.ChatCompletionResponse{
				Choices: []wire.Choice{{Message: wire.Message{Role: wire.RoleAssistant, Content: wire.TextContent("summary")}, FinishReason: wire.FinishStop}},
				Usage:   &wire.Usage{PromptTokens: 50, CompletionTokens: 10},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if streams.next() == 0 {
			writeSSEChunk(t, w, wire.ChatCompletionChunk{Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{
				Role: "assistant",
				ToolCalls: []wire.ToolCallDelta{
					{Index: 0, ID: "call_w", Type: "function", Function: wire.ToolCallFuncDelta{Name: "write", Arguments: `{"file_path":"deploy.sh","content":"echo deploy\n"}`}},
					{Index: 1, ID: "call_b", Type: "function", Function: wire.ToolCallFuncDelta{Name: "bash", Arguments: `{"command":"echo before the fork"}`}},
				},
			}}}})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishToolCalls)}},
				Usage:   &wire.Usage{PromptTokens: 900, PromptCacheMissTokens: 900, CompletionTokens: 5},
			})
		} else {
			writeSSEChunk(t, w, wire.ChatCompletionChunk{Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{Role: "assistant", Content: strPtr("done")}}}})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishStop)}},
				Usage:   &wire.Usage{PromptTokens: 300, PromptCacheMissTokens: 300, CompletionTokens: 5},
			})
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	r := newTestRunner(t, srv.URL)
	r.CompactionThresholdTokens = 500
	ws := t.TempDir()
	res, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "write a script",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	child, err := r.Store.GetSession(t.Context(), res.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if child.ParentID == "" {
		t.Fatal("the run did not compact")
	}

	own, err := r.Store.GetEvents(t.Context(), child.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range own {
		if e.Kind == store.KindToolCall {
			t.Fatalf("the successor's own log holds a %s event; the chain walk is not needed", e.Kind)
		}
	}

	h, err := LoadGuardHistory(t.Context(), r.Store, child)
	if err != nil {
		t.Fatalf("LoadGuardHistory: %v", err)
	}
	for _, e := range h.Events {
		if !slices.Contains(guardEventKinds, e.Kind) {
			t.Errorf("history holds a %s event, which the guard state never reads", e.Kind)
		}
	}
	parent, _ := r.Store.GetSession(t.Context(), child.ParentID)
	if !h.Start.Equal(parent.CreatedAt) {
		t.Errorf("Start = %v, want the first session's creation %v", h.Start, parent.CreatedAt)
	}

	in := GuardInputs(ws, h, GuardCall{ID: "call_next", Command: "bash deploy.sh"})
	if len(in.Commands) != 1 || in.Commands[0] != "echo before the fork" {
		t.Errorf("Commands = %q, want the command from before the fork", in.Commands)
	}
	if len(in.Mutations) != 1 || !in.Mutations[0].Created || filepath.Base(in.Mutations[0].Path) != "deploy.sh" {
		t.Fatalf("Mutations = %+v, want the write from before the fork", in.Mutations)
	}

	st := BuildGuardState(t.Context(), guardBuilder(t, h.Start), ws, h, GuardCall{ID: "call_next", Command: "bash deploy.sh"})
	if len(st.ScriptsRun) != 1 || st.ScriptsRun[0].Origin != guard.OriginWrittenThisSession ||
		st.ScriptsRun[0].Content == nil || *st.ScriptsRun[0].Content != "echo deploy\n" {
		t.Fatalf("a script written before the fork = %+v, want written_this_session with its content", st.ScriptsRun)
	}
	if reasons := guard.NewScriptDetector().EscalationReasons("bash deploy.sh", st.ScriptsRun); len(reasons) != 0 {
		t.Fatalf("a readable single script escalated: %v", reasons)
	}
}

func TestGuardEventsRecordsDecisionAndStage2Usage(t *testing.T) {
	prices := &pricing.Table{CapturedAt: "2026-11-03", Models: map[string]pricing.ModelPrices{
		"invented-stage2-model": {InputCacheHitPerMillionUSD: 0.01, InputCacheMissPerMillionUSD: 0.10, InputCacheWritePerMillionUSD: 0.125, OutputPerMillionUSD: 0.50},
	}}
	pb := 0.4
	d := guard.Decision{
		Verdict: guard.VerdictBlock, DecidedBy: guard.DecidedByStage2, PBlock: &pb, Stage2Ran: true,
		Stage2Model: "invented-stage2-model", Stage2Reason: "an invented reason", GuardVersion: "v1",
		Stage2Usage: &guard.Usage{InputTokens: 1000, OutputTokens: 200, CacheCreationInputTokens: 4000, CacheReadInputTokens: 2000},
		Latency:     1500 * time.Millisecond,
	}
	inputs := GuardEvents("call_1", "shadow", d, prices, time.Now())
	if len(inputs) != 2 || inputs[0].Kind != store.KindGuardDecision || inputs[1].Kind != store.KindUsage {
		t.Fatalf("events = %+v, want a decision then a usage event", inputs)
	}
	dec := inputs[0].Payload.(store.GuardDecisionPayload)
	if dec.ToolCallID != "call_1" || dec.Mode != "shadow" || dec.Verdict != "block" || dec.LatencyMs != 1500 || dec.Stage2Usage.CacheCreationInputTokens != 4000 {
		t.Errorf("decision payload = %+v", dec)
	}
	u := inputs[1].Payload.(store.UsagePayload)
	if u.Model != "invented-stage2-model" || u.PromptTokens != 7000 || u.PromptCacheMissTokens != 1000 || u.PromptCacheHitTokens != 2000 || u.PromptCacheWriteTokens != 4000 || u.CompletionTokens != 200 {
		t.Errorf("usage payload = %+v", u)
	}
	wantCost := (1000*0.10 + 2000*0.01 + 4000*0.125 + 200*0.50) / 1e6
	if diff := u.CostUSD - wantCost; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("cost = %v, want %v", u.CostUSD, wantCost)
	}

	d.Stage2Usage = nil
	if got := GuardEvents("call_2", "shadow", d, prices, time.Now()); len(got) != 1 {
		t.Errorf("a decision with no stage 2 usage should record one event, got %d", len(got))
	}
}
