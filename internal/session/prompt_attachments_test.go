package session

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRenderOpeningMessageNamesAttachments pins the contract that lets the
// model find the files a request carried: the opening message lists each
// attachment under scratch/attachments/, ahead of the task, with the path a
// tool call can use — so the model can pass the mockup to Glance
// instead of trying to describe it.
func TestRenderOpeningMessageNamesAttachments(t *testing.T) {
	msg := RenderOpeningMessage("/ws", "make the page match the mockup", nil, "", "", []string{"mockup.png", "light.webp"})

	if !strings.Contains(msg, "Image files attached to this task, materialised into scratch/attachments/") {
		t.Errorf("opening message should introduce the attachments, got: %s", msg)
	}
	for _, want := range []string{"- scratch/attachments/mockup.png", "- scratch/attachments/light.webp", "Glance"} {
		if !strings.Contains(msg, want) {
			t.Errorf("opening message should carry %q, got: %s", want, msg)
		}
	}
	// The task text stays last, so the model reads the task after the data
	// that describes the workspace.
	if !strings.HasSuffix(msg, "Task:\nmake the page match the mockup\n") {
		t.Errorf("task should come last, got: %s", msg)
	}
}

// TestRenderOpeningMessageWithoutAttachmentsIsUnchanged pins that a run
// with no attachments produces byte-identical text to a run before the
// feature existed — the opening message is part of the conversation, and an
// empty attachment list must not perturb it.
func TestRenderOpeningMessageWithoutAttachmentsIsUnchanged(t *testing.T) {
	withNil := RenderOpeningMessage("/ws", "do the thing", json.RawMessage(`{"type":"object"}`), "claude-md", "skills", nil)
	withEmpty := RenderOpeningMessage("/ws", "do the thing", json.RawMessage(`{"type":"object"}`), "claude-md", "skills", []string{})
	if withNil != withEmpty {
		t.Fatal("a nil and an empty attachment list must render identically")
	}
	if strings.Contains(withNil, "scratch/attachments") {
		t.Errorf("no attachments must mean no attachments block, got: %s", withNil)
	}
}
