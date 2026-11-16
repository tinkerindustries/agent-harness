package fold

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/store"
	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// TestFoldBackgroundNoticeNamesTheSessionsOwnTool pins that a background
// shell's exit notice points the model at the call it was offered: a Claude
// session's frozen array names Claude Code's TaskOutput, so its notice says
// TaskOutput and task_id; a session in the harness's own names keeps
// job_output and job_id, byte for byte the Message every parent-facing
// translator renders.
func TestFoldBackgroundNoticeNamesTheSessionsOwnTool(t *testing.T) {
	notice := store.BackgroundFinishedPayload{BashID: "bash_1", Command: "scripts/build.sh"}
	events := func() []store.Event {
		b := &eventBuilder{}
		return []store.Event{
			b.ev(store.KindSessionStarted, store.SessionStartedPayload{OpeningMessage: "run the gate"}),
			b.ev(store.KindBackgroundFinished, notice),
		}
	}
	lastUserText := func(items []wire.Item) string {
		return items[len(items)-1].Content.String()
	}

	claude := testSession()
	schema, err := json.Marshal([]wire.Tool{
		{Type: "function", Function: wire.ToolFunction{Name: "Bash"}},
		{Type: "function", Function: wire.ToolFunction{Name: "TaskOutput"}},
		{Type: "function", Function: wire.ToolFunction{Name: "TaskStop"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	claude.ToolSchema = schema
	items, err := Fold(claude, events())
	if err != nil {
		t.Fatal(err)
	}
	got := lastUserText(items)
	if !strings.Contains(got, `readable with TaskOutput using task_id "bash_1"`) || strings.Contains(got, "job_output") {
		t.Errorf("Claude session's notice = %q, want it to name TaskOutput and task_id", got)
	}

	items, err = Fold(testSession(), events())
	if err != nil {
		t.Fatal(err)
	}
	if got := lastUserText(items); got != notice.Message() {
		t.Errorf("canonical session's notice = %q, want %q", got, notice.Message())
	}
}
