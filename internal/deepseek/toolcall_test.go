package deepseek

import (
	"encoding/json"
	"testing"
)

// unmarshalDelta parses a raw JSON tool-call delta, the shape observed on
// the wire (docs/OBSERVED.md), the same way the streaming decoder does.
func unmarshalDelta(t *testing.T, raw string) ToolCallDelta {
	t.Helper()
	var d ToolCallDelta
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatalf("unmarshal delta %s: %v", raw, err)
	}
	return d
}

func TestToolCallAssemblerFragmentedArguments(t *testing.T) {
	// Fragments as observed: an opening frame carrying identity and an
	// empty argument string, then argument-only frames that split
	// mid-token and mid-string.
	raws := []string{
		`{"index":0,"id":"call_00_Xwh0","type":"function","function":{"name":"get_weather","arguments":""}}`,
		`{"index":0,"function":{"arguments":"{"}}`,
		`{"index":0,"function":{"arguments":"\""}}`,
		`{"index":0,"function":{"arguments":"location"}}`,
		`{"index":0,"function":{"arguments":"\""}}`,
		`{"index":0,"function":{"arguments":":"}}`,
		`{"index":0,"function":{"arguments":"\""}}`,
		`{"index":0,"function":{"arguments":"H"}}`,
		`{"index":0,"function":{"arguments":"ob"}}`,
		`{"index":0,"function":{"arguments":"art"}}`,
		`{"index":0,"function":{"arguments":"\""}}`,
		`{"index":0,"function":{"arguments":"}"}}`,
	}

	asm := NewToolCallAssembler()
	for _, raw := range raws {
		asm.Add(unmarshalDelta(t, raw))
	}

	got := asm.Finalize()
	if len(got) != 1 {
		t.Fatalf("Finalize() returned %d calls, want 1", len(got))
	}
	call := got[0]
	if call.ID != "call_00_Xwh0" {
		t.Errorf("ID = %q, want call_00_Xwh0", call.ID)
	}
	if call.Type != "function" {
		t.Errorf("Type = %q, want function", call.Type)
	}
	if call.Name != "get_weather" {
		t.Errorf("Name = %q, want get_weather", call.Name)
	}
	wantArgs := `{"location":"Hobart"}`
	if call.Arguments != wantArgs {
		t.Errorf("Arguments = %q, want %q", call.Arguments, wantArgs)
	}

	var parsed map[string]string
	if err := json.Unmarshal([]byte(call.Arguments), &parsed); err != nil {
		t.Fatalf("assembled arguments are not valid JSON: %v", err)
	}
	if parsed["location"] != "Hobart" {
		t.Errorf("parsed location = %q, want Hobart", parsed["location"])
	}
}

func TestToolCallAssemblerParallelCalls(t *testing.T) {
	// Two calls interleaved by index, mirroring the parallel-call example
	// in docs/OBSERVED.md: "What is the weather in Hobart, and also in
	// Perth?" producing calls at index 0 and index 1.
	raws := []string{
		`{"index":0,"id":"call_00_a","type":"function","function":{"name":"get_weather","arguments":""}}`,
		`{"index":1,"id":"call_01_b","type":"function","function":{"name":"get_weather","arguments":""}}`,
		`{"index":0,"function":{"arguments":"{\"location\""}}`,
		`{"index":1,"function":{"arguments":"{\"location\""}}`,
		`{"index":0,"function":{"arguments":":\"Hobart\"}"}}`,
		`{"index":1,"function":{"arguments":":\"Perth\"}"}}`,
	}

	asm := NewToolCallAssembler()
	for _, raw := range raws {
		asm.Add(unmarshalDelta(t, raw))
	}

	got := asm.Finalize()
	if len(got) != 2 {
		t.Fatalf("Finalize() returned %d calls, want 2", len(got))
	}

	if got[0].ID != "call_00_a" || got[0].Arguments != `{"location":"Hobart"}` {
		t.Errorf("call 0 = %+v", got[0])
	}
	if got[1].ID != "call_01_b" || got[1].Arguments != `{"location":"Perth"}` {
		t.Errorf("call 1 = %+v", got[1])
	}
}

func TestToolCallAssemblerEmpty(t *testing.T) {
	asm := NewToolCallAssembler()
	got := asm.Finalize()
	if len(got) != 0 {
		t.Errorf("Finalize() on empty assembler returned %d calls, want 0", len(got))
	}
}

// The malformations below are the shapes seen in production wire logs,
// reduced to their structure: one closing brace short, one closing brace
// too many, and a brace emitted one key early. All were on flash, on
// arguments objects of seven to eleven kilobytes, and all stopped cleanly
// with finish_reason "tool_calls" (docs/OBSERVED.md). The first two are
// repaired; the third has two readings and is left alone.
func TestRepairArguments(t *testing.T) {
	cases := []struct {
		name         string
		finishReason string
		args         string
		want         string
		wantOK       bool
	}{{
		name:         "one closing brace short",
		finishReason: FinishToolCalls,
		args:         `{"summary":"done","checks":[{"outcome":"passed"}]`,
		want:         `{"summary":"done","checks":[{"outcome":"passed"}]}`,
		wantOK:       true,
	}, {
		name:         "one closing brace too many",
		finishReason: FinishToolCalls,
		args:         `{"summary":"done","result":{"error":""}}}`,
		want:         `{"summary":"done","result":{"error":""}}`,
		wantOK:       true,
	}, {
		// The object closed early and the model carried on with the key
		// it had not written yet. Deleting either brace parses, and the
		// two disagree about whether "error" belongs inside result or
		// beside it, so this is left for the executor's schema to judge.
		name:         "closing one key early is ambiguous and left alone",
		finishReason: FinishToolCalls,
		args:         `{"result":{"added":61},"error":""},"summary":"phase 2 done"}`,
		want:         `{"result":{"added":61},"error":""},"summary":"phase 2 done"}`,
		wantOK:       false,
	}, {
		// Braces inside string values are not structure. This shape is
		// from a real payload: a Bash command with a Go template in it,
		// inside arguments that were one brace short.
		name:         "braces inside strings are not counted",
		finishReason: FinishToolCalls,
		args:         `{"command":"docker ps --format '{{.Names}}' | grep prod","outcome":"passed"`,
		want:         `{"command":"docker ps --format '{{.Names}}' | grep prod","outcome":"passed"}`,
		wantOK:       true,
	}, {
		name:         "valid arguments are untouched",
		finishReason: FinishToolCalls,
		args:         `{"summary":"done"}`,
		want:         `{"summary":"done"}`,
		wantOK:       false,
	}, {
		// The guard that matters. Truncated arguments are one brace
		// short too, and balancing them would present a partial result
		// to a tool as a complete one.
		name:         "truncation is not repaired",
		finishReason: FinishLength,
		args:         `{"summary":"done","checks":[{"outcome":"passed"}]`,
		want:         `{"summary":"done","checks":[{"outcome":"passed"}]`,
		wantOK:       false,
	}, {
		name:         "two levels short is not guessed at",
		finishReason: FinishToolCalls,
		args:         `{"summary":"done","checks":[{"outcome":"passed"`,
		want:         `{"summary":"done","checks":[{"outcome":"passed"`,
		wantOK:       false,
	}, {
		name:         "ending inside a string is not repaired",
		finishReason: FinishToolCalls,
		args:         `{"summary":"the run was cut off mid-sen`,
		want:         `{"summary":"the run was cut off mid-sen`,
		wantOK:       false,
	}, {
		name:         "empty arguments are left alone",
		finishReason: FinishToolCalls,
		args:         "",
		want:         "",
		wantOK:       false,
	}, {
		name:         "a stray closer is not repaired",
		finishReason: FinishToolCalls,
		args:         `{"summary":"done"]}`,
		want:         `{"summary":"done"]}`,
		wantOK:       false,
	}, {
		// Repairing to a JSON value that is not an object has not
		// reconstructed a tool's arguments.
		name:         "a repair that is not an object is rejected",
		finishReason: FinishToolCalls,
		args:         `["a","b"`,
		want:         `["a","b"`,
		wantOK:       false,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := RepairArguments(tc.finishReason, tc.args)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("RepairArguments(%q, %q)\n got  = %q, %v\n want = %q, %v",
					tc.finishReason, tc.args, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// Whatever a repair produces has to be something the executor can act on,
// which the narrow rules above are meant to guarantee rather than merely
// tend towards.
func TestRepairArgumentsAlwaysProducesValidObject(t *testing.T) {
	inputs := []string{
		`{"a":1`,
		`{"a":{"b":2}`,
		`{"a":1}}`,
		`{"a":1},"b":2}`, // ambiguous: left alone
		`{"a":"}"`,
		`{"a":"\""`,
		`{"a":[1,2]`,
		"",
		"   ",
		"null",
		`{"a":1`,
		`not json at all`,
		`{{{{`,
		`}}}}`,
	}
	for _, in := range inputs {
		got, ok := RepairArguments(FinishToolCalls, in)
		if !ok {
			if got != in {
				t.Errorf("RepairArguments(%q) reported no repair but changed the text to %q", in, got)
			}
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(got), &obj); err != nil || obj == nil {
			t.Errorf("RepairArguments(%q) = %q, which is not a JSON object (%v)", in, got, err)
		}
	}
}
