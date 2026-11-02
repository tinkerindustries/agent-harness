package session

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/store"
	"github.com/mrgeoffrich/agent-harness/internal/tools"
	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// startedPayloadOf runs one session against srvURL in ws and returns the
// session_started payload the run stored.
func startedPayloadOf(t *testing.T, srvURL, ws string) store.SessionStartedPayload {
	t.Helper()
	r := newTestRunner(t, srvURL)
	res, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "say something",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	events, err := r.Store.GetEvents(t.Context(), res.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var started store.SessionStartedPayload
	if err := json.Unmarshal(events[0].Payload, &started); err != nil {
		t.Fatal(err)
	}
	return started
}

// A skill shipped in a cloned repository reaches the model through the
// opening message, ahead of the task text.
func TestRunListsWorkspaceSkills(t *testing.T) {
	srv := plainAnswerServer(t, "all done")
	defer srv.Close()

	ws := t.TempDir()
	dir := filepath.Join(ws, "myrepo", ".claude", "skills", "test-runner")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: test-runner\ndescription: Run this repository's suite the way CI does.\n---\n\nRun scripts/test.sh.\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	started := startedPayloadOf(t, srv.URL, ws)
	opening := started.OpeningMessage
	wantPath := "myrepo/.claude/skills/test-runner/SKILL.md"
	if !strings.Contains(opening, wantPath) {
		t.Fatalf("opening message does not name the skill file:\n%s", opening)
	}
	if !strings.Contains(opening, "Run this repository's suite the way CI does.") {
		t.Fatalf("opening message does not carry the skill description:\n%s", opening)
	}
	if strings.Index(opening, wantPath) > strings.Index(opening, "Task:") {
		t.Fatalf("the skill catalogue should precede the task text:\n%s", opening)
	}

	// The browser lifts the catalogue into its own panel by removing this
	// exact substring from the opening message, so it has to be one.
	if started.SkillCatalogue == "" {
		t.Fatal("session_started should carry the catalogue separately")
	}
	if !strings.Contains(opening, started.SkillCatalogue) {
		t.Fatalf("skill_catalogue is not a substring of the opening message:\n%q", started.SkillCatalogue)
	}
}

// A workspace with no skills leaves the opening message exactly as it was
// before skills existed, so the shared prefix is undisturbed (docs/CACHE.md).
func TestRunWithoutSkillsLeavesOpeningMessageUnchanged(t *testing.T) {
	srv := plainAnswerServer(t, "all done")
	defer srv.Close()

	ws := t.TempDir()
	resolved, err := tools.ResolvePath(ws, ".")
	if err != nil {
		t.Fatal(err)
	}
	started := startedPayloadOf(t, srv.URL, ws)
	if want := RenderOpeningMessage(resolved, "say something", nil, "", "", "", nil); started.OpeningMessage != want {
		t.Fatalf("opening message = %q, want %q", started.OpeningMessage, want)
	}
	if started.SkillCatalogue != "" {
		t.Fatalf("skill_catalogue should be empty, got %q", started.SkillCatalogue)
	}
}

// A skill installed for the user lives outside the workspace, so the
// catalogue names it absolutely and the run has to allow Read to open it.
// This drives the whole path: discovery, the opening message, the model's
// Read call, and the body coming back as the tool result.
func TestRunReadsASkillInstalledOutsideTheWorkspace(t *testing.T) {
	config := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", config)

	skillDir := filepath.Join(config, "skills", "ship-it")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	skillPath := filepath.Join(skillDir, "SKILL.md")
	body := "---\nname: ship-it\ndescription: Land the work.\n---\n\nRun the chain.\n"
	if err := os.WriteFile(skillPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	resolvedSkill, err := filepath.EvalSymlinks(skillPath)
	if err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			args, _ := json.Marshal(map[string]string{"file_path": skillPath})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{
					Role: "assistant",
					ToolCalls: []wire.ToolCallDelta{{Index: 0, ID: "call-read", Type: "function",
						Function: wire.ToolCallFuncDelta{Name: "Read", Arguments: string(args)}}},
				}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishToolCalls)}},
			})
		} else {
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{Role: "assistant", Content: strPtr("shipped")}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishStop)}},
			})
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	r := newTestRunner(t, srv.URL)
	ws := t.TempDir()
	res, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "ship it",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != store.StatusOK {
		t.Fatalf("expected status ok, got %s", res.Status)
	}

	events, err := r.Store.GetEvents(t.Context(), res.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var sawCatalogue, sawBody bool
	for _, event := range events {
		switch event.Kind {
		case store.KindSessionStarted:
			var payload store.SessionStartedPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			sawCatalogue = strings.Contains(payload.SkillCatalogue, resolvedSkill)
		case store.KindToolResult:
			var payload store.ToolResultPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Name == "Read" && !payload.IsError && strings.Contains(payload.Content, "Run the chain.") {
				sawBody = true
			}
		}
	}
	if !sawCatalogue {
		t.Fatalf("the opening message did not name the installed skill at %s", resolvedSkill)
	}
	if !sawBody {
		t.Fatal("the Read call did not return the skill body")
	}
}
