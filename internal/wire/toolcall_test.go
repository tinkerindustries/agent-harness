package wire

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
