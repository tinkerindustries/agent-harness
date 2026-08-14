// Package geminitest builds the event-stream bodies a stubbed Gemini server
// returns, so every package that fakes one states the wire format in one
// place rather than three.
//
// The vocabulary is recorded from a live streamed interaction on 2026-08-14;
// internal/gemini/stream.go decodes it and testdata there holds the captures
// themselves.
package geminitest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Step is one step of a stubbed stream: its type, the text deltas it emits,
// and any thought_summary deltas.
type Step struct {
	Type      string
	Texts     []string
	Summaries []string
}

// Answer is the common stub: one model_output step carrying text, with an
// optional usage object on the completed frame. Pass "" for no usage.
func Answer(text, usageJSON string) string {
	return Stream([]Step{{Type: "model_output", Texts: []string{text}}}, usageJSON)
}

// Stream builds the frames for steps: interaction.created, an
// interaction.status_update the published examples do not show, a
// step.start / step.delta… / step.stop group per step, interaction.completed
// carrying the usage, and a done frame whose data is [DONE].
//
// The two delta shapes nest their text differently: a text delta carries
// delta.text, a thought_summary delta carries delta.content.text.
func Stream(steps []Step, usageJSON string) string {
	var b strings.Builder
	frame := func(event, data string) {
		fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", event, data)
	}
	frame("interaction.created", `{"interaction":{"id":"v1_test","status":"in_progress"},"event_type":"interaction.created"}`)
	frame("interaction.status_update", `{"interaction_id":"v1_test","status":"in_progress","event_type":"interaction.status_update"}`)
	for i, s := range steps {
		frame("step.start", fmt.Sprintf(`{"index":%d,"step":{"type":%q},"event_type":"step.start"}`, i, s.Type))
		if s.Type == "thought" {
			frame("step.delta", fmt.Sprintf(`{"index":%d,"delta":{"signature":"sig","type":"thought_signature"},"event_type":"step.delta"}`, i))
		}
		for _, text := range s.Summaries {
			frame("step.delta", fmt.Sprintf(`{"index":%d,"delta":{"content":{"text":%s,"type":"text"},"type":"thought_summary"},"event_type":"step.delta"}`, i, quote(text)))
		}
		for _, text := range s.Texts {
			frame("step.delta", fmt.Sprintf(`{"index":%d,"delta":{"text":%s,"type":"text"},"event_type":"step.delta"}`, i, quote(text)))
		}
		frame("step.stop", fmt.Sprintf(`{"index":%d,"event_type":"step.stop"}`, i))
	}
	completed := `{"interaction":{"id":"v1_test","status":"completed"`
	if usageJSON != "" {
		// A data line cannot carry a raw newline, so an indented fixture is
		// compacted rather than rejected.
		var flat bytes.Buffer
		if err := json.Compact(&flat, []byte(usageJSON)); err != nil {
			panic("geminitest: usage fixture is not JSON: " + err.Error())
		}
		completed += `,"usage":` + flat.String()
	}
	completed += `},"event_type":"interaction.completed"}`
	frame("interaction.completed", completed)
	frame("done", "[DONE]")
	return b.String()
}

func quote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic("geminitest: quote: " + err.Error())
	}
	return string(b)
}
