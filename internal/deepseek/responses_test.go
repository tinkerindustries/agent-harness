package deepseek

import (
	"encoding/json"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// probeIntent is the shape every sub-turn after the first sends: the frozen
// system prompt, the opening message, an assistant turn carrying both
// reasoning and a tool call, and that call's result. It is the intent the
// live probes in docs/OBSERVED.md were run with.
func probeIntent() wire.ChatIntent {
	const reasoning = "The user wants the file listing. I should call List."
	return wire.ChatIntent{
		Model:     "deepseek-v4-flash",
		Thinking:  true,
		Effort:    wire.EffortLow,
		MaxTokens: 2000,
		Items: []wire.Item{
			wire.SystemItem("You are a terse assistant."),
			wire.UserItem("List the files, then say DONE."),
			wire.ReasoningItem(reasoning),
			wire.FunctionCallItem("call_1", "List", `{"path":"."}`),
			wire.FunctionCallOutputItem("call_1", "a.go\nb.go", ""),
		},
		Tools: []wire.Tool{{
			Type: "function",
			Function: wire.ToolFunction{
				Name:        "List",
				Description: "List files at a path.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
			},
		}},
	}
}

// TestResponsesRequestBody pins the exact bytes of a representative request.
// This is the Responses surface's half of internal/wire's golden request
// test, and it exists for the same reason: the head of every request is the
// frozen prefix the prompt cache is built on, so a field that moves, gains
// an omitempty, or changes name costs a full-price re-read of every running
// session's conversation (docs/DESIGN.md §3.2, docs/CACHE.md).
//
// The body below is the one accepted by the live API on 2026-09-10
// (docs/OBSERVED.md, "DeepSeek Responses API").
func TestResponsesRequestBody(t *testing.T) {
	got, err := json.Marshal(responsesRequestFromIntent(probeIntent()))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `{"model":"deepseek-v4-flash",` +
		`"instructions":"You are a terse assistant.",` +
		`"input":[` +
		`{"type":"message","role":"user","content":"List the files, then say DONE."},` +
		`{"type":"reasoning","content":[{"type":"reasoning_text","text":"The user wants the file listing. I should call List."}]},` +
		`{"type":"function_call","call_id":"call_1","name":"List","arguments":"{\"path\":\".\"}"},` +
		`{"type":"function_call_output","call_id":"call_1","output":"a.go\nb.go"}` +
		`],` +
		`"reasoning":{"effort":"low"},` +
		`"max_output_tokens":2000,` +
		`"tools":[{"type":"function","name":"List","description":"List files at a path.","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}]}`
	if string(got) != want {
		t.Errorf("request body drifted.\n got: %s\nwant: %s", got, want)
	}
}

// The `input` list is the intent's own items, unchanged. This is the
// property the whole arrangement exists for: the loop's conversation
// vocabulary is this surface's, so building a request means lifting the
// system item into `instructions` and serialising the rest as they stand
// (internal/wire/item.go, docs/DEEPSEEK-RESPONSES.md).
//
// It is asserted as identity rather than field by field on purpose: a
// future change that reintroduced a per-item rebuild would still pass a
// shape test and would fail this one.
func TestInputIsTheIntentsItemsUnchanged(t *testing.T) {
	intent := probeIntent()
	req := responsesRequestFromIntent(intent)

	if req.Instructions != "You are a terse assistant." {
		t.Errorf("instructions = %q", req.Instructions)
	}
	want := intent.Items[1:] // everything after the system item
	if len(req.Input) != len(want) {
		t.Fatalf("input has %d items, want the intent's %d", len(req.Input), len(want))
	}
	for i := range want {
		gotJSON, _ := json.Marshal(req.Input[i])
		wantJSON, _ := json.Marshal(want[i])
		if string(gotJSON) != string(wantJSON) {
			t.Errorf("item %d was rebuilt on the way out.\n got: %s\nwant: %s", i, gotJSON, wantJSON)
		}
	}

	// The order the fold puts them in is the order that matters: the
	// reasoning item ahead of the call it explains, because DeepSeek answers
	// 400 without it on a request carrying tools
	// (third_party/deepseek-docs/guides/thinking_mode.md, "Tool Calls").
	kinds := make([]string, len(req.Input))
	for i, item := range req.Input {
		kinds[i] = item.Type
	}
	wantKinds := []string{wire.ItemMessage, wire.ItemReasoning, wire.ItemFunctionCall, wire.ItemFunctionCallOutput}
	for i, w := range wantKinds {
		if kinds[i] != w {
			t.Errorf("item %d is %q, want %q (kinds: %v)", i, kinds[i], w, kinds)
		}
	}
}

// An image in a tool result rides as an input_image part in the item's own
// `output`, with image_url a bare string — this surface's envelope, where
// Chat Completions wraps the same URL in an object. It is also the one the
// vendor documents: the Chat Completions shape is contradicted by DeepSeek's
// own schema (docs/DEEPSEEK-VISION.md §2).
//
// The item is built by wire.FunctionCallOutputItem and sent as it stands, so
// what this pins is the bytes that leave the process.
func TestToolResultCarriesAnImagePart(t *testing.T) {
	item := wire.FunctionCallOutputItem("call_1", "Screenshot saved", "data:image/png;base64,AAA")

	body, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `{"type":"function_call_output","call_id":"call_1","output":[` +
		`{"type":"input_text","text":"Screenshot saved"},` +
		`{"type":"input_image","image_url":"data:image/png;base64,AAA"}]}`
	if string(body) != want {
		t.Errorf("item body drifted.\n got: %s\nwant: %s", body, want)
	}
}

// Text is typed by direction on this surface: what the model reads is
// input_text and what it wrote is output_text. Sending the wrong one is the
// kind of mistake that costs a 400.
func TestTextPartDirection(t *testing.T) {
	user, _ := json.Marshal(wire.UserItem("look"))
	if got := string(user); got != `{"type":"message","role":"user","content":"look"}` {
		t.Errorf("user item = %s", got)
	}

	assistant, _ := json.Marshal(wire.AssistantItem("looked"))
	const wantAssistant = `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"looked"}]}`
	if got := string(assistant); got != wantAssistant {
		t.Errorf("assistant item = %s, want %s", got, wantAssistant)
	}
}

// The two reasoning controls the loop states separately are one field here,
// and "none" is the off position rather than an effort.
func TestResponsesEffort(t *testing.T) {
	cases := []struct {
		name     string
		thinking bool
		effort   string
		want     string
	}{
		{"thinking off is none", false, wire.EffortMax, effortNone},
		{"thinking off with no effort", false, "", effortNone},
		{"an effort passes through", true, wire.EffortLow, wire.EffortLow},
		{"max passes through", true, wire.EffortMax, wire.EffortMax},
		{"no effort is the API's own default", true, "", wire.EffortHigh},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := responsesEffort(wire.ChatIntent{Thinking: tc.thinking, Effort: tc.effort})
			if got != tc.want {
				t.Errorf("responsesEffort = %q, want %q", got, tc.want)
			}
		})
	}
}

// A request with no tools omits the field rather than sending an empty list:
// an empty array is a byte the frozen prefix would carry for nothing.
func TestNoToolsOmitsTheField(t *testing.T) {
	body, _ := json.Marshal(responsesRequestFromIntent(wire.ChatIntent{
		Model: "deepseek-v4-flash", Thinking: true,
		Items: []wire.Item{wire.SystemItem("s"), wire.UserItem("hi")},
	}))
	if got := string(body); contains(got, `"tools"`) {
		t.Errorf("an empty tool array reached the wire: %s", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// The cache split is derived here, not read: this surface reports one input
// total with the hit nested under it, so the miss is the remainder.
func TestUsageFromResponses(t *testing.T) {
	if usageFromResponses(nil) != nil {
		t.Error("a nil usage should stay nil")
	}

	var u responsesUsage
	u.InputTokens = 357
	u.InputTokensDetails.CachedTokens = 256
	u.OutputTokens = 3
	u.OutputTokensDetails.ReasoningTokens = 27
	u.TotalTokens = 360

	got := usageFromResponses(&u)
	if got.PromptTokens != 357 || got.PromptCacheHitTokens != 256 || got.PromptCacheMissTokens != 101 {
		t.Errorf("prompt figures = %d/%d/%d, want 357/256/101",
			got.PromptTokens, got.PromptCacheHitTokens, got.PromptCacheMissTokens)
	}
	if got.CompletionTokens != 3 || got.TotalTokens != 360 {
		t.Errorf("completion/total = %d/%d, want 3/360", got.CompletionTokens, got.TotalTokens)
	}
	if got.CompletionTokensDetails == nil || got.CompletionTokensDetails.ReasoningTokens != 27 {
		t.Errorf("reasoning tokens = %+v, want 27", got.CompletionTokensDetails)
	}

	// A hit larger than the total would make the miss negative, which every
	// figure downstream reads as a credit. Clamped rather than trusted.
	var odd responsesUsage
	odd.InputTokens = 10
	odd.InputTokensDetails.CachedTokens = 99
	if miss := usageFromResponses(&odd).PromptCacheMissTokens; miss != 0 {
		t.Errorf("miss = %d, want it clamped to 0", miss)
	}
}

// The loop reads one finish-reason vocabulary whatever surface it is on, so
// a status plus an incomplete reason has to arrive as one of its strings.
func TestFinishReasonFor(t *testing.T) {
	cases := []struct {
		name string
		resp *responseObject
		want string
	}{
		{"nil is a plain stop", nil, wire.FinishStop},
		{"completed with text", &responseObject{Status: statusCompleted}, wire.FinishStop},
		{"completed with a call", &responseObject{Status: statusCompleted,
			Output: []outputItem{{Type: wire.ItemMessage}, {Type: wire.ItemFunctionCall}}}, wire.FinishToolCalls},
		{"truncated", &responseObject{Status: statusIncomplete,
			IncompleteDetails: &incompleteDetails{Reason: "max_output_tokens"}}, wire.FinishLength},
		{"filtered", &responseObject{Status: statusIncomplete,
			IncompleteDetails: &incompleteDetails{Reason: "content_filter"}}, wire.FinishContentFilter},
		{"incomplete for an unknown reason is still truncation",
			&responseObject{Status: statusIncomplete}, wire.FinishLength},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := finishReasonFor(tc.resp); got != tc.want {
				t.Errorf("finishReasonFor = %q, want %q", got, tc.want)
			}
		})
	}
}

// An intent with no token ceiling omits the field. Zero is not a legal
// value: DeepSeek answers `max_output_tokens: 0` with "the valid range of
// max_tokens is [1, 393216]", so a session whose create named no ceiling
// would fail on its first sub-turn (docs/OBSERVED.md).
func TestNoTokenCeilingOmitsTheField(t *testing.T) {
	in := probeIntent()
	in.MaxTokens = 0
	body, _ := json.Marshal(responsesRequestFromIntent(in))
	if contains(string(body), "max_output_tokens") {
		t.Errorf("a zero ceiling reached the wire: %s", body)
	}

	in.MaxTokens = 2000
	body, _ = json.Marshal(responsesRequestFromIntent(in))
	if !contains(string(body), `"max_output_tokens":2000`) {
		t.Errorf("a real ceiling did not reach the wire: %s", body)
	}
}
