package gemini

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// TestRequestFromIntentMessageRoles pins the role mapping
// docs/GEMINI-INTEGRATION.md §3 and §5.3 describe: system becomes the
// top-level system_instruction and never appears in Input; user becomes a
// user_input step; an assistant message carrying a ThoughtSignature becomes
// a thought step ahead of whatever it produced (docs/OBSERVED.md, "the
// parallel-call signature rule holds"); assistant tool calls become
// function_call steps with Arguments as a genuine object, not the JSON
// string wire carries it as; an assistant message with no tool calls
// becomes a model_output step; and a tool result becomes a function_result
// step carrying the call's name, recovered from the function_call step that
// introduced its call_id.
func TestRequestFromIntentMessageRoles(t *testing.T) {
	sig := "sig-abc"
	intent := wire.ChatIntent{
		Model: "gemini-3.7-flash",
		Items: []wire.Item{
			wire.SystemItem("you are a coding agent"),
			wire.UserItem("list the files"),
			signedReasoning(sig),
			wire.FunctionCallItem("call_01", "List", `{"path":"."}`),
			wire.FunctionCallOutputItem("call_01", "a.go\nb.go", ""),
			wire.AssistantItem("Found two files."),
		},
		Effort: wire.EffortHigh,
	}

	req := requestFromIntent(intent)

	if req.SystemInstruction != "you are a coding agent" {
		t.Errorf("system_instruction = %q, want the system message text", req.SystemInstruction)
	}
	// user_input, thought, function_call, function_result, model_output —
	// the system message contributes no step of its own.
	if len(req.Input) != 5 {
		t.Fatalf("input has %d steps, want 5, got %+v", len(req.Input), req.Input)
	}
}

// TestRequestFromIntentStepShapes exercises each step kind's exact fields,
// split out from the role test above so a failure names which shape broke.
func TestRequestFromIntentStepShapes(t *testing.T) {
	sig := "sig-xyz"
	intent := wire.ChatIntent{
		Model: "gemini-3.7-flash",
		Items: []wire.Item{
			wire.SystemItem("sys"),
			wire.UserItem("do the thing"),
			signedReasoning(sig),
			wire.FunctionCallItem("call_01", "get_weather", `{"location":"Hobart, Tasmania"}`),
			wire.FunctionCallOutputItem("call_01", "Sunny, 18C", ""),
			wire.AssistantItem("It's sunny in Hobart."),
		},
	}
	req := requestFromIntent(intent)
	if len(req.Input) != 5 {
		t.Fatalf("input has %d steps, want 5 (user_input, thought, function_call, function_result, model_output), got %+v", len(req.Input), req.Input)
	}

	userStep, ok := req.Input[0].(UserInputStep)
	if !ok {
		t.Fatalf("input[0] = %T, want UserInputStep", req.Input[0])
	}
	if userStep.Type != StepTypeUserInput || len(userStep.Content) != 1 || userStep.Content[0].Text != "do the thing" {
		t.Errorf("user step = %+v, want user_input carrying %q", userStep, "do the thing")
	}

	thoughtStep, ok := req.Input[1].(ThoughtStep)
	if !ok {
		t.Fatalf("input[1] = %T, want ThoughtStep", req.Input[1])
	}
	if thoughtStep.Type != StepTypeThought || thoughtStep.Signature != sig {
		t.Errorf("thought step = %+v, want signature %q", thoughtStep, sig)
	}

	callStep, ok := req.Input[2].(FunctionCallStep)
	if !ok {
		t.Fatalf("input[2] = %T, want FunctionCallStep", req.Input[2])
	}
	if callStep.Type != StepTypeFunctionCall || callStep.ID != "call_01" || callStep.Name != "get_weather" {
		t.Errorf("function_call step = %+v, want id call_01, name get_weather", callStep)
	}
	if string(callStep.Arguments) != `{"location":"Hobart, Tasmania"}` {
		t.Errorf("function_call arguments = %s, want the object form of the wire string, byte for byte", callStep.Arguments)
	}

	resultStep, ok := req.Input[3].(FunctionResultStep)
	if !ok {
		t.Fatalf("input[3] = %T, want FunctionResultStep", req.Input[3])
	}
	if resultStep.Type != StepTypeFunctionResult || resultStep.CallID != "call_01" {
		t.Errorf("function_result step = %+v, want call_id call_01", resultStep)
	}
	if resultStep.Name != "get_weather" {
		t.Errorf("function_result name = %q, want get_weather recovered from the function_call step", resultStep.Name)
	}
	if len(resultStep.Result) != 1 || resultStep.Result[0].Type != ContentTypeText || resultStep.Result[0].Text != "Sunny, 18C" {
		t.Errorf("function_result result = %+v, want one text block", resultStep.Result)
	}

	outputStep, ok := req.Input[4].(ModelOutputStep)
	if !ok {
		t.Fatalf("input[4] = %T, want ModelOutputStep", req.Input[4])
	}
	if outputStep.Type != StepTypeModelOutput || len(outputStep.Content) != 1 || outputStep.Content[0].Text != "It's sunny in Hobart." {
		t.Errorf("model_output step = %+v, want the final text", outputStep)
	}
}

// TestRequestFromIntentParallelToolCalls pins the shape docs/OBSERVED.md's
// "the parallel-call signature rule holds" measured: one thought step
// carrying the only signature, then one function_call step per call, none
// of them carrying a signature of their own — the fold's own assistant
// message shape for parallel calls (internal/fold/fold_test.go,
// TestFoldParallelToolCallTurn) has exactly one ThoughtSignature-bearing
// message with several ToolCalls, which is what this reads.
func TestRequestFromIntentParallelToolCalls(t *testing.T) {
	sig := "sig-parallel"
	intent := wire.ChatIntent{
		Model: "gemini-3.7-flash",
		Items: []wire.Item{
			wire.SystemItem("sys"),
			wire.UserItem("check the weather in three cities"),
			signedReasoning(sig),
			wire.FunctionCallItem("call_00_a", "get_weather", `{"location":"Hobart"}`),
			wire.FunctionCallItem("call_01_b", "get_weather", `{"location":"Perth"}`),
			wire.FunctionCallItem("call_02_c", "get_weather", `{"location":"Darwin"}`),
		},
	}
	req := requestFromIntent(intent)
	// user_input, thought, then three function_call steps: exactly one
	// thought step for the whole turn, not one per call.
	if len(req.Input) != 5 {
		t.Fatalf("input has %d steps, want 5, got %+v", len(req.Input), req.Input)
	}
	if _, ok := req.Input[1].(ThoughtStep); !ok {
		t.Fatalf("input[1] = %T, want the turn's single ThoughtStep", req.Input[1])
	}
	for i, wantID := range []string{"call_00_a", "call_01_b", "call_02_c"} {
		call, ok := req.Input[2+i].(FunctionCallStep)
		if !ok {
			t.Fatalf("input[%d] = %T, want FunctionCallStep", 2+i, req.Input[2+i])
		}
		if call.ID != wantID {
			t.Errorf("input[%d].ID = %q, want %q", 2+i, call.ID, wantID)
		}
	}
}

// TestRequestFromIntentFunctionResultImage pins the shape a Read call that
// returned an image produces once it reaches the Gemini request builder
// (docs/GEMINI-INTEGRATION.md §5.7, §7 "Phase 7"). internal/fold's
// toolResultMessage builds exactly this parts shape for a tool_result event
// carrying ImageURL — a text part with the label first, then the image_url
// part with the raw data URI (internal/fold/fold.go, toolResultMessage) —
// so this intent is what a folded session with a vision-capable model
// actually sends. Asserts the serialised function_result step byte for
// byte: text block then image block, in that order, mime_type and data
// split correctly out of the data URI, and no resolution key at all — the
// "unspecified" default this harness deliberately never overrides for a
// single ad hoc tool-result image (see this test's package doc note below
// and docs/GEMINI-INTEGRATION.md §7's resolution decision).
func TestRequestFromIntentFunctionResultImage(t *testing.T) {
	const pngData = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="
	intent := wire.ChatIntent{
		Model: "gemini-3.7-flash",
		Items: []wire.Item{
			wire.SystemItem("sys"),
			wire.UserItem("take a screenshot and check it"),
			wire.FunctionCallItem("call_01", "Read", `{"file_path":"scratch/screenshot.png"}`),
			wire.FunctionCallOutputItem("call_01", "Image: scratch/screenshot.png", "data:image/png;base64,"+pngData),
		},
	}

	req := requestFromIntent(intent)
	// user_input, function_call, function_result — no thought step, this
	// message carries no ThoughtSignature.
	if len(req.Input) != 3 {
		t.Fatalf("input has %d steps, want 3, got %+v", len(req.Input), req.Input)
	}
	resultStep, ok := req.Input[2].(FunctionResultStep)
	if !ok {
		t.Fatalf("input[2] = %T, want FunctionResultStep", req.Input[2])
	}
	if len(resultStep.Result) != 2 {
		t.Fatalf("function_result carries %d blocks, want 2 (text label, image)", len(resultStep.Result))
	}
	if resultStep.Result[0].Type != ContentTypeText || resultStep.Result[0].Text != "Image: scratch/screenshot.png" {
		t.Errorf("block 0 = %+v, want the text label first", resultStep.Result[0])
	}
	img := resultStep.Result[1]
	if img.Type != ContentTypeImage || img.MIMEType != "image/png" || img.Data != pngData {
		t.Errorf("block 1 = %+v, want image/png carrying the base64 payload unchanged", img)
	}
	if img.Resolution != "" {
		t.Errorf("image block resolution = %q, want unset — Read/MCP images are one ad hoc image per tool result, not the multi-image batch Glance/Ground/Detect scrutinise", img.Resolution)
	}

	// The actual bytes on the wire, not just the struct: mime_type and data
	// as their own keys, image block after the text block, and no
	// resolution key at all (omitempty on an empty string).
	raw, err := json.Marshal(resultStep)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"function_result","call_id":"call_01","name":"Read","result":[{"type":"text","text":"Image: scratch/screenshot.png"},{"type":"image","mime_type":"image/png","data":"` + pngData + `"}]}`
	if string(raw) != want {
		t.Fatalf("function_result step =\n%s\nwant:\n%s", raw, want)
	}
}

// TestRequestFromIntentFunctionResultImageFromMCP is
// TestRequestFromIntentFunctionResultImage's counterpart for the other
// producer of an image tool result: execMCP (internal/tools/mcpexec.go)
// builds the identical "data:<mime>;base64,<...>" URI from an MCP server's
// image content block, through the same ImageURL field and the same fold
// path, so it reaches requestFromIntent as the same wire shape. This test
// exercises a JPEG (Read only ever emits PNG/JPEG/WebP too, but a real MCP
// server is not bounded to those three) with MCP-flavoured label text, to
// pin that the conversion is not special-cased to Read's own wording.
func TestRequestFromIntentFunctionResultImageFromMCP(t *testing.T) {
	const jpegData = "/9j/4AAQSkZJRgABAQEAYABgAAD/2wBDAAMCAgICAgMCAgIDAwMDBAYEBAQEBAgGBgUGCQgKCgkI"
	intent := wire.ChatIntent{
		Model: "gemini-3.7-flash",
		Items: []wire.Item{
			wire.SystemItem("sys"),
			wire.UserItem("render the viewport"),
			wire.FunctionCallItem("call_09", "mcp__blender__render_viewport_to_path", `{}`),
			wire.FunctionCallOutputItem("call_09", "Wrote scratch/mcp/blender-render_viewport_to_path-1.jpg", "data:image/jpeg;base64,"+jpegData),
		},
	}

	req := requestFromIntent(intent)
	resultStep, ok := req.Input[2].(FunctionResultStep)
	if !ok {
		t.Fatalf("input[2] = %T, want FunctionResultStep", req.Input[2])
	}
	if len(resultStep.Result) != 2 {
		t.Fatalf("function_result carries %d blocks, want 2 (text label, image)", len(resultStep.Result))
	}
	if resultStep.Result[0].Type != ContentTypeText || resultStep.Result[0].Text != "Wrote scratch/mcp/blender-render_viewport_to_path-1.jpg" {
		t.Errorf("block 0 = %+v, want the MCP write-location text first", resultStep.Result[0])
	}
	img := resultStep.Result[1]
	if img.Type != ContentTypeImage || img.MIMEType != "image/jpeg" || img.Data != jpegData {
		t.Errorf("block 1 = %+v, want image/jpeg carrying the base64 payload unchanged", img)
	}
}

// TestRequestFromIntentOmitsToolChoiceAndSamplingParams pins the three
// deliberate omissions docs/GEMINI-INTEGRATION.md §3, §5.3 and
// docs/OBSERVED.md's "tool_choice" section name: no tool_choice anywhere
// (nested or top-level), no temperature/top_p/top_k, and store is always
// present and false rather than omitted — the API's own default is
// store:true, so an absent field would silently opt into it.
func TestRequestFromIntentOmitsToolChoiceAndSamplingParams(t *testing.T) {
	intent := wire.ChatIntent{
		Model:  "gemini-3.7-flash",
		Items:  []wire.Item{wire.SystemItem("sys"), wire.UserItem("hi")},
		Effort: wire.EffortHigh,
	}
	raw, err := json.Marshal(requestFromIntent(intent))
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, forbidden := range []string{"tool_choice", "temperature", "top_p", "top_k", "thinking_budget"} {
		if contains(body, forbidden) {
			t.Errorf("request body carries %q; it must never be sent", forbidden)
		}
	}
	if !contains(body, `"store":false`) {
		t.Errorf("request body must carry an explicit store:false, got: %s", body)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (func() bool {
		for i := 0; i+len(substr) <= len(s); i++ {
			if s[i:i+len(substr)] == substr {
				return true
			}
		}
		return false
	})()
}

// TestThinkingLevelFromEffort pins the effort-to-thinking_level mapping:
// wire's "low" and "high" pass through unchanged (they are the same strings
// ThinkingLevelLow and ThinkingLevelHigh already spell), "max" collapses to
// Gemini's highest level since Gemini's enum has no "max", empty omits
// generation_config entirely, and a Gemini-native level ("medium",
// "minimal") passes through verbatim for a caller that already knows the
// right spelling. This mapping is a guess — see intent.go's own comment on
// thinkingLevelFromEffort for why no source pins it.
func TestThinkingLevelFromEffort(t *testing.T) {
	cases := []struct{ effort, want string }{
		{wire.EffortLow, ThinkingLevelLow},
		{wire.EffortHigh, ThinkingLevelHigh},
		{wire.EffortMax, ThinkingLevelHigh},
		{"", ""},
		{ThinkingLevelMedium, ThinkingLevelMedium},
		{ThinkingLevelMinimal, ThinkingLevelMinimal},
	}
	for _, c := range cases {
		if got := thinkingLevelFromEffort(c.effort); got != c.want {
			t.Errorf("thinkingLevelFromEffort(%q) = %q, want %q", c.effort, got, c.want)
		}
	}

	// An empty mapped level must omit the thinking_level field, leaving it
	// to the API's default. generation_config itself is still sent, because
	// thinking_summaries always rides on it.
	req := requestFromIntent(wire.ChatIntent{Model: "gemini-3.7-flash", Items: []wire.Item{wire.UserItem("hi")}})
	if req.GenerationConfig == nil || req.GenerationConfig.ThinkingLevel != "" {
		t.Errorf("generation_config = %+v, want an empty thinking_level when Effort is empty", req.GenerationConfig)
	}
	if raw, err := json.Marshal(req); err != nil {
		t.Fatal(err)
	} else if contains(string(raw), "thinking_level") {
		t.Errorf("request body carries thinking_level for an empty Effort, got: %s", raw)
	}
	req = requestFromIntent(wire.ChatIntent{Model: "gemini-3.7-flash", Items: []wire.Item{wire.UserItem("hi")}, Effort: wire.EffortHigh})
	if req.GenerationConfig == nil || req.GenerationConfig.ThinkingLevel != ThinkingLevelHigh {
		t.Errorf("generation_config = %+v, want thinking_level high", req.GenerationConfig)
	}
}

// TestRequestFromIntentMaxTokens pins that intent.MaxTokens rides
// generation_config.max_output_tokens: a live measurement found the field
// honoured (max_output_tokens=50 answered 200 with status "incomplete" and
// 46 output tokens, against 678 uncapped), contradicting an earlier line in
// docs/OBSERVED.md that recorded a failed search for it as an absence.
// MaxTokens zero must omit the field, the same way an empty effort omits
// thinking_level, so a caller that never sets a ceiling gets the API's own
// default rather than an explicit but meaningless zero.
func TestRequestFromIntentMaxTokens(t *testing.T) {
	req := requestFromIntent(wire.ChatIntent{
		Model: "gemini-3.7-flash", Items: []wire.Item{wire.UserItem("hi")}, MaxTokens: 4096,
	})
	if req.GenerationConfig == nil || req.GenerationConfig.MaxOutputTokens != 4096 {
		t.Fatalf("generation_config = %+v, want max_output_tokens 4096", req.GenerationConfig)
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(string(raw), `"max_output_tokens":4096`) {
		t.Errorf("request body does not carry max_output_tokens, got: %s", raw)
	}

	zero := requestFromIntent(wire.ChatIntent{Model: "gemini-3.7-flash", Items: []wire.Item{wire.UserItem("hi")}})
	if zero.GenerationConfig == nil || zero.GenerationConfig.MaxOutputTokens != 0 {
		t.Errorf("generation_config = %+v, want max_output_tokens unset when MaxTokens is zero", zero.GenerationConfig)
	}
	rawZero, err := json.Marshal(zero)
	if err != nil {
		t.Fatal(err)
	}
	if contains(string(rawZero), "max_output_tokens") {
		t.Errorf("request body carries max_output_tokens for a zero MaxTokens, got: %s", rawZero)
	}

	// MaxTokens and Effort combine into the one generation_config object,
	// rather than either field forcing the other to a zero value.
	both := requestFromIntent(wire.ChatIntent{
		Model: "gemini-3.7-flash", Items: []wire.Item{wire.UserItem("hi")},
		MaxTokens: 8000, Effort: wire.EffortHigh,
	})
	if both.GenerationConfig == nil || both.GenerationConfig.MaxOutputTokens != 8000 || both.GenerationConfig.ThinkingLevel != ThinkingLevelHigh {
		t.Errorf("generation_config = %+v, want both max_output_tokens 8000 and thinking_level high", both.GenerationConfig)
	}
}

// TestRequestFromIntentThinkingSummaries pins that every agentic request
// asks for thought summaries. Without generation_config.thinking_summaries
// the API returns thought steps carrying a signature and no summary, so a
// run's reasoning reaches the store as empty text and the UI shows no
// thinking at all — the symptom measured on
// sess-35da6920ca8e5ca590acb3a46341c924, whose 102 reasoning events were
// every one of them empty while usage reported thought tokens each turn.
// The signature is what must be replayed and is unaffected either way
// (chat_types.go, ThoughtStep); the summary is display text, and this is
// the only request field that produces it.
func TestRequestFromIntentThinkingSummaries(t *testing.T) {
	for _, intent := range []wire.ChatIntent{
		{Model: "gemini-3.7-flash", Items: []wire.Item{wire.UserItem("hi")}},
		{Model: "gemini-3.7-flash", Items: []wire.Item{wire.UserItem("hi")}, Effort: wire.EffortMax},
		{Model: "gemini-3.7-flash", Items: []wire.Item{wire.UserItem("hi")}, MaxTokens: 4096},
	} {
		req := requestFromIntent(intent)
		if req.GenerationConfig == nil || req.GenerationConfig.ThinkingSummaries != ThinkingSummariesAuto {
			t.Fatalf("generation_config = %+v, want thinking_summaries %q for intent %+v",
				req.GenerationConfig, ThinkingSummariesAuto, intent)
		}
		raw, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		if !contains(string(raw), `"thinking_summaries":"auto"`) {
			t.Errorf("request body does not carry thinking_summaries, got: %s", raw)
		}
	}
}

// TestArgumentsRoundTripByteStable pins §5.6's byte-stability requirement:
// converting wire's JSON-string arguments into Gemini's JSON-object
// arguments and back must not reorder an object's keys, which a
// map[string]any round trip would do. json.RawMessage carried straight
// through is what makes that true; this test would catch a regression to a
// map-based conversion by construction, since Go map key order is
// randomised and a reordering would eventually show up across runs even if
// one run got lucky.
func TestArgumentsRoundTripByteStable(t *testing.T) {
	// Key order deliberately not alphabetical, so a map round trip would be
	// caught reordering it (encoding/json sorts map keys alphabetically on
	// marshal, which would move "zebra" ahead of "apple").
	const args = `{"zebra":"stripes","apple":"red","nested":{"z":1,"a":2}}`

	obj := argumentsToObject(args)
	if string(obj) != args {
		t.Fatalf("argumentsToObject(%s) = %s, want it unchanged", args, obj)
	}
	back := argumentsFromObject(obj)
	if back != args {
		t.Fatalf("argumentsFromObject(%s) = %s, want the original string back, byte for byte", obj, back)
	}

	// An empty call (assembled with no arguments frames) becomes the
	// smallest valid JSON object rather than an empty, invalid byte slice —
	// FunctionCallStep.Arguments is a required field on the wire.
	if got := argumentsToObject(""); string(got) != "{}" {
		t.Errorf("argumentsToObject(\"\") = %s, want {}", got)
	}
}

// TestToolsFromWireFlattensSchema pins the flattened tool shape
// docs/GEMINI-INTEGRATION.md §3 and openapi.json's "Function" schema both
// describe: name, description, and parameters directly on the tool object,
// no nested function wrapper the way OpenAI-format carries it, and
// Parameters stays the same json.RawMessage bytes wire.ToolFunction already
// holds rather than being re-marshalled through a map.
func TestToolsFromWireFlattensSchema(t *testing.T) {
	params := json.RawMessage(`{"type":"object","properties":{"location":{"type":"string"}},"required":["location"]}`)
	tools := []wire.Tool{
		{Type: "function", Function: wire.ToolFunction{Name: "get_weather", Description: "Get the weather", Parameters: params}},
	}
	out := toolsFromWire(tools)
	if len(out) != 1 {
		t.Fatalf("got %d tools, want 1", len(out))
	}
	if out[0].Type != "function" || out[0].Name != "get_weather" || out[0].Description != "Get the weather" {
		t.Errorf("tool = %+v, want the flattened fields", out[0])
	}
	if string(out[0].Parameters) != string(params) {
		t.Errorf("parameters = %s, want %s byte for byte", out[0].Parameters, params)
	}

	raw, err := json.Marshal(out[0])
	if err != nil {
		t.Fatal(err)
	}
	if contains(string(raw), `"function":{`) {
		t.Errorf("tool serialised with a nested function object, want it flattened: %s", raw)
	}
}

// goldenChatRequest builds the fully-populated agentic request whose
// serialised bytes TestChatRequestBodyGolden pins, following how
// internal/wire/request_golden_test.go's own goldenRequest does: a system
// message, a user message, an assistant turn carrying a thought signature
// and a tool call, a tool result, a final assistant text turn, and a
// two-entry tool array. Tool definitions are hand-built here rather than
// read from internal/tools.Definitions(): that package already imports
// internal/gemini for the vision path (internal/tools/vision.go), so
// importing it back from a gemini test would be a cycle.
func goldenChatRequest() ChatInteractionRequest {
	sig := "EpoGCpcGAXLI2nx-golden-signature"
	params := json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)
	intent := wire.ChatIntent{
		Model: "gemini-3.7-flash",
		Items: []wire.Item{
			wire.SystemItem("You are a helpful coding agent."),
			wire.UserItem("What is in this workspace?"),
			signedReasoning(sig),
			wire.FunctionCallItem("call_01", "List", `{"path":"."}`),
			wire.FunctionCallOutputItem("call_01", "README.md\nmain.go", ""),
			wire.AssistantItem("This workspace has a README and main.go."),
		},
		Effort:    wire.EffortHigh,
		MaxTokens: 48000,
		Tools: []wire.Tool{
			{Type: "function", Function: wire.ToolFunction{Name: "List", Description: "List files in a directory.", Parameters: params}},
			{Type: "function", Function: wire.ToolFunction{Name: "Read", Description: "Read a file.", Parameters: params}},
		},
	}
	return requestFromIntent(intent)
}

// TestChatRequestBodyGolden pins the exact bytes of a fully-populated
// agentic request, the same contract internal/wire/request_golden_test.go
// pins for DeepSeek and Kimi: the request head is frozen and the prompt
// cache depends on identical bytes for an identical intent
// (docs/DESIGN.md §3.2, docs/GEMINI-INTEGRATION.md §5.6).
func TestChatRequestBodyGolden(t *testing.T) {
	got, err := json.Marshal(goldenChatRequest())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "chat_request_body.golden.json"))
	if err != nil {
		t.Fatalf("ReadFile golden: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("request body differs from golden file:\ngot:  %s\nwant: %s", got, want)
	}
}

// signedReasoning is the item a Gemini sub-turn's thought step becomes: no
// text, and the signature that must be replayed verbatim.
func signedReasoning(sig string) wire.Item {
	item := wire.ReasoningItem("")
	item.ThoughtSignature = sig
	return item
}
