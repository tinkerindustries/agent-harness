package session

import (
	"strings"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

func TestCompactionInstructionNamesEverySection(t *testing.T) {
	for _, heading := range []string{
		"## Intent", "## User corrections and constraints", "## Files", "## Errors and fixes",
		"## Work completed", "## Current work", "## Pending work", "## Next action", "## Critical details",
	} {
		if !strings.Contains(compactionInstruction, heading) {
			t.Errorf("compaction instruction lacks %q", heading)
		}
	}
	if !strings.Contains(compactionInstruction, "previous checkpoint") {
		t.Error("compaction instruction does not say how to merge a previous checkpoint")
	}
}

func TestCompactionTranscript(t *testing.T) {
	long := strings.Repeat("x", maxCompactionCallArgs+50)
	items := []wire.Item{
		wire.UserItem("fix the build"),
		{Type: wire.ItemReasoning},
		{Type: wire.ItemFunctionCall, Name: "bash", Arguments: `{"command":"go test ./..."}`},
		{Type: wire.ItemFunctionCallOutput, Output: wire.TextItemContent("ok")},
		{Type: wire.ItemFunctionCall, Name: "apply_patch", Arguments: long},
	}
	got := compactionTranscript(items, "")
	for _, want := range []string{
		"[user] fix the build\n",
		`[call] bash {"command":"go test ./..."}` + "\n",
		"[tool] ok\n",
		"…(truncated)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("transcript lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, long) {
		t.Error("a call's arguments were not truncated")
	}
	if strings.Contains(got, "previous checkpoint") {
		t.Error("a transcript with no checkpoint mentions one")
	}
}

func TestCompactionTranscriptLeadsWithCheckpoint(t *testing.T) {
	got := compactionTranscript([]wire.Item{wire.UserItem("next")}, "## Intent\nold goal")
	if !strings.HasPrefix(got, "[previous checkpoint]\n## Intent\nold goal\n[end of previous checkpoint]") {
		t.Errorf("checkpoint is not first:\n%s", got)
	}
}

func TestPriorCheckpointRoundTrip(t *testing.T) {
	prompt := RenderCompactionSummarySystemPromptFor("deepseek-flash", "", "## Intent\nship it")
	if got := priorCheckpoint(prompt); got != "## Intent\nship it" {
		t.Errorf("priorCheckpoint = %q", got)
	}
	if got := priorCheckpoint("an ordinary system prompt"); got != "" {
		t.Errorf("priorCheckpoint of a plain prompt = %q", got)
	}
}
