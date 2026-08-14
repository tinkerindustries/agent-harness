package gemini

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// The streaming half of the interactions surface. The request carries
// stream: true and the response is text/event-stream; frames are
// "event: <name>\ndata: <json>\n\n" and the stream ends with an event named
// done whose data is the literal [DONE].
//
// Streaming is how a review call stays inside its deadline. The unary form
// sends nothing until the model has finished thinking, which for a
// multi-image review reaches 29s
// (docs/reviews/sess-8df2a5f78847c4737b0e86e6e5d069b6.md); streamed, the
// first frame arrives immediately and the connection is never idle.
//
// The frame vocabulary below was measured against the live API on
// 2026-08-14, and it is wider than the published examples: an
// interaction.status_update event appears that the docs do not show, and the
// two delta shapes nest their text differently — a text delta carries
// delta.text, a thought_summary delta carries delta.content.text.

// Stream event names.
const (
	eventInteractionCreated   = "interaction.created"
	eventInteractionCompleted = "interaction.completed"
	eventStepStart            = "step.start"
	eventStepDelta            = "step.delta"
	eventDone                 = "done"
)

// Delta types inside a step.delta frame. Anything else is ignored: the
// surface carries tool-call and image deltas this client never asks for.
const (
	deltaText           = "text"
	deltaThoughtSummary = "thought_summary"
)

// streamFrame is one decoded SSE frame's data payload. The fields are the
// union of what the frames this client cares about carry; each event name
// populates a different subset.
type streamFrame struct {
	EventType   string           `json:"event_type"`
	Index       int              `json:"index"`
	Step        *streamStep      `json:"step"`
	Delta       *streamDelta     `json:"delta"`
	Interaction *streamInteraion `json:"interaction"`
}

type streamStep struct {
	Type string `json:"type"`
}

// streamDelta is one increment of a step. Text is set on a text delta;
// Content is set on a thought_summary delta, which nests its text one level
// deeper.
type streamDelta struct {
	Type    string   `json:"type"`
	Text    string   `json:"text"`
	Content *Content `json:"content"`
}

// streamInteraion is the interaction object carried by the created and
// completed frames.
type streamInteraion struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Usage  *Usage `json:"usage"`
}

// maxStreamLine bounds one SSE data line. A thought signature runs to about
// 1.5KB and a text delta is smaller, so this is headroom rather than a
// working limit.
const maxStreamLine = 1 << 20

// decodeStream reads an event stream and assembles the same
// InteractionResponse the unary form returns, so nothing downstream of
// Interact can tell which transport produced it. Steps are indexed by the
// frames' own index, and a stream that ends without an
// interaction.completed frame is an error: a truncated answer must not be
// mistaken for a short one.
func decodeStream(body io.Reader) (*InteractionResponse, error) {
	out := &InteractionResponse{}
	steps := map[int]*Step{}
	highest := -1
	completed := false

	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), maxStreamLine)

	var event, data string
	flush := func() error {
		if event == "" && data == "" {
			return nil
		}
		defer func() { event, data = "", "" }()
		if event == eventDone || strings.TrimSpace(data) == "[DONE]" {
			return nil
		}
		var f streamFrame
		if err := json.Unmarshal([]byte(data), &f); err != nil {
			return fmt.Errorf("gemini: decode %s frame: %w", event, err)
		}
		switch event {
		case eventInteractionCreated:
			if f.Interaction != nil {
				out.ID = f.Interaction.ID
				out.Status = f.Interaction.Status
			}
		case eventStepStart:
			s := &Step{}
			if f.Step != nil {
				s.Type = f.Step.Type
			}
			steps[f.Index] = s
			if f.Index > highest {
				highest = f.Index
			}
		case eventStepDelta:
			s, ok := steps[f.Index]
			if !ok {
				// A delta before its step.start: keep the content rather than
				// drop it, and let the step's type stay empty.
				s = &Step{}
				steps[f.Index] = s
				if f.Index > highest {
					highest = f.Index
				}
			}
			applyDelta(s, f.Delta)
		case eventInteractionCompleted:
			completed = true
			if f.Interaction != nil {
				if f.Interaction.ID != "" {
					out.ID = f.Interaction.ID
				}
				out.Status = f.Interaction.Status
				out.Usage = f.Interaction.Usage
			}
		}
		return nil
	}

	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if err := flush(); err != nil {
				return nil, err
			}
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data += strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("gemini: read stream: %w", err)
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if !completed {
		return nil, fmt.Errorf("gemini: stream ended after %d step(s) without an %s frame", highest+1, eventInteractionCompleted)
	}

	for i := 0; i <= highest; i++ {
		if s, ok := steps[i]; ok {
			out.Steps = append(out.Steps, *s)
		}
	}
	return out, nil
}

// applyDelta folds one step.delta onto its step, concatenating consecutive
// text so the assembled step holds one part per kind rather than one per
// frame.
func applyDelta(s *Step, d *streamDelta) {
	if d == nil {
		return
	}
	switch d.Type {
	case deltaText:
		appendText(&s.Content, d.Text)
	case deltaThoughtSummary:
		if d.Content != nil {
			appendText(&s.Summary, d.Content.Text)
		}
	}
}

func appendText(parts *[]Content, text string) {
	if text == "" {
		return
	}
	if n := len(*parts); n > 0 && (*parts)[n-1].Type == ContentTypeText {
		(*parts)[n-1].Text += text
		return
	}
	*parts = append(*parts, Content{Type: ContentTypeText, Text: text})
}
