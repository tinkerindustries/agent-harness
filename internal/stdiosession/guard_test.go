package stdiosession

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/store"
)

type capturedFrame struct {
	method string
	params any
}

func guardEventFor(t *testing.T, p store.GuardDecisionPayload) store.Event {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return store.Event{SessionID: "sess_1", Seq: 9, Kind: store.KindGuardDecision, Payload: raw, CreatedAt: time.Now()}
}

// TestResponsesTranslatorEmitsGuardDecision commits a decision by hand and
// reads the harness.guard frame it becomes.
func TestResponsesTranslatorEmitsGuardDecision(t *testing.T) {
	pb := 0.42
	event := guardEventFor(t, store.GuardDecisionPayload{
		ToolCallID: "call_7", Mode: "shadow", GuardVersion: "v-test", Verdict: "block", DecidedBy: "stage2",
		PBlock: &pb, Escalation: []string{"unscreened_script"}, Stage2Ran: true,
		Stage2Model: "claude-haiku-5-5", Stage2Reason: "pushes to the default branch",
		Stage2Usage:     &store.GuardUsage{InputTokens: 10, OutputTokens: 20, CacheCreationInputTokens: 30, CacheReadInputTokens: 40},
		Stage2LatencyMs: 900, LatencyMs: 950,
	})

	var frames []capturedFrame
	tr := newTranslator("resp_1", "deepseek-flash", func(method string, params any) {
		frames = append(frames, capturedFrame{method, params})
	})
	tr.Event(event)

	if len(frames) != 1 || frames[0].method != "harness.guard" {
		t.Fatalf("frames = %+v, want one harness.guard", frames)
	}
	raw, err := json.Marshal(frames[0].params)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	want := `{"call_id":"call_7","guard":{"decided_by":"stage2","escalation":["unscreened_script"],"guard_version":"v-test","latency_ms":950,"mode":"shadow","p_block":0.42,"stage2":{"latency_ms":900,"model":"claude-haiku-5-5","reason":"pushes to the default branch","usage":{"cache_creation_input_tokens":30,"cache_read_input_tokens":40,"input_tokens":10,"output_tokens":20}},"verdict":"block"},"response_id":"resp_1","sequence_number":0,"type":"harness.guard"}`
	wantMap := map[string]any{}
	if err := json.Unmarshal([]byte(want), &wantMap); err != nil {
		t.Fatal(err)
	}
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(wantMap)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("frame =\n%s\nwant\n%s", gotJSON, wantJSON)
	}
}

func TestResponsesGuardFrameForRulesBlockOmitsStages(t *testing.T) {
	event := guardEventFor(t, store.GuardDecisionPayload{
		ToolCallID: "call_8", Mode: "shadow", GuardVersion: "v-test", Verdict: "block", DecidedBy: "rules",
		Rules: []string{"push_default_branch"}, LatencyMs: 1,
	})
	var frames []capturedFrame
	tr := newTranslator("resp_1", "deepseek-flash", func(method string, params any) {
		frames = append(frames, capturedFrame{method, params})
	})
	tr.Event(event)
	raw, _ := json.Marshal(frames[0].params)
	var got struct {
		Guard map[string]json.RawMessage `json:"guard"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"stage2", "p_block", "escalation", "stage1_error"} {
		if _, ok := got.Guard[key]; ok {
			t.Errorf("a rules decision carries %q", key)
		}
	}
	if string(got.Guard["rules"]) != `["push_default_branch"]` {
		t.Errorf("rules = %s", got.Guard["rules"])
	}
}

// TestOtherVocabulariesSkipGuardDecision pins that the Interactions and
// Managed Agents translators send nothing for the event.
func TestOtherVocabulariesSkipGuardDecision(t *testing.T) {
	event := guardEventFor(t, store.GuardDecisionPayload{ToolCallID: "call_9", Mode: "shadow", Verdict: "allow", DecidedBy: "stage1"})

	var iact []capturedFrame
	newIactTranslator("int_1", "gemini-3.7-flash", func(m string, p any) { iact = append(iact, capturedFrame{m, p}) }).Event(event)
	if len(iact) != 0 {
		t.Errorf("Interactions translator emitted %+v", iact)
	}

	var ma []capturedFrame
	newMATranslator("turn_1", "sess_1", "claude-sonnet-5-5", func(m string, p any) { ma = append(ma, capturedFrame{m, p}) }).Event(event)
	if len(ma) != 0 {
		t.Errorf("Managed Agents translator emitted %+v", ma)
	}
}
