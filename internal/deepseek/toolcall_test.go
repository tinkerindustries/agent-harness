package deepseek

import (
	"encoding/json"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// The malformations below are the shapes seen in production wire logs,
// reduced to their structure: one closing brace short, one closing brace
// too many, and a brace emitted one key early. All were on flash, on
// arguments objects of seven to eleven kilobytes, and all stopped cleanly
// with finish_reason "tool_calls" (docs/OBSERVED.md). The first two are
// repaired; the third has two readings and is left alone.
func TestRepairArguments(t *testing.T) {
	client := NewClient("http://unused.invalid", "test-key")
	cases := []struct {
		name         string
		finishReason string
		args         string
		want         string
		wantOK       bool
	}{{
		name:         "one closing brace short",
		finishReason: wire.FinishToolCalls,
		args:         `{"summary":"done","checks":[{"outcome":"passed"}]`,
		want:         `{"summary":"done","checks":[{"outcome":"passed"}]}`,
		wantOK:       true,
	}, {
		name:         "one closing brace too many",
		finishReason: wire.FinishToolCalls,
		args:         `{"summary":"done","result":{"error":""}}}`,
		want:         `{"summary":"done","result":{"error":""}}`,
		wantOK:       true,
	}, {
		// The object closed early and the model carried on with the key
		// it had not written yet. Deleting either brace parses, and the
		// two disagree about whether "error" belongs inside result or
		// beside it, so this is left for the executor's schema to judge.
		name:         "closing one key early is ambiguous and left alone",
		finishReason: wire.FinishToolCalls,
		args:         `{"result":{"added":61},"error":""},"summary":"phase 2 done"}`,
		want:         `{"result":{"added":61},"error":""},"summary":"phase 2 done"}`,
		wantOK:       false,
	}, {
		// Braces inside string values are not structure. This shape is
		// from a real payload: a Bash command with a Go template in it,
		// inside arguments that were one brace short.
		name:         "braces inside strings are not counted",
		finishReason: wire.FinishToolCalls,
		args:         `{"command":"docker ps --format '{{.Names}}' | grep prod","outcome":"passed"`,
		want:         `{"command":"docker ps --format '{{.Names}}' | grep prod","outcome":"passed"}`,
		wantOK:       true,
	}, {
		name:         "valid arguments are untouched",
		finishReason: wire.FinishToolCalls,
		args:         `{"summary":"done"}`,
		want:         `{"summary":"done"}`,
		wantOK:       false,
	}, {
		// The guard that matters. Truncated arguments are one brace
		// short too, and balancing them would present a partial result
		// to a tool as a complete one.
		name:         "truncation is not repaired",
		finishReason: wire.FinishLength,
		args:         `{"summary":"done","checks":[{"outcome":"passed"}]`,
		want:         `{"summary":"done","checks":[{"outcome":"passed"}]`,
		wantOK:       false,
	}, {
		name:         "two levels short is not guessed at",
		finishReason: wire.FinishToolCalls,
		args:         `{"summary":"done","checks":[{"outcome":"passed"`,
		want:         `{"summary":"done","checks":[{"outcome":"passed"`,
		wantOK:       false,
	}, {
		name:         "ending inside a string is not repaired",
		finishReason: wire.FinishToolCalls,
		args:         `{"summary":"the run was cut off mid-sen`,
		want:         `{"summary":"the run was cut off mid-sen`,
		wantOK:       false,
	}, {
		name:         "empty arguments are left alone",
		finishReason: wire.FinishToolCalls,
		args:         "",
		want:         "",
		wantOK:       false,
	}, {
		name:         "a stray closer is not repaired",
		finishReason: wire.FinishToolCalls,
		args:         `{"summary":"done"]}`,
		want:         `{"summary":"done"]}`,
		wantOK:       false,
	}, {
		// Repairing to a JSON value that is not an object has not
		// reconstructed a tool's arguments.
		name:         "a repair that is not an object is rejected",
		finishReason: wire.FinishToolCalls,
		args:         `["a","b"`,
		want:         `["a","b"`,
		wantOK:       false,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := client.RepairArguments(tc.finishReason, tc.args)
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
	client := NewClient("http://unused.invalid", "test-key")
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
		got, ok := client.RepairArguments(wire.FinishToolCalls, in)
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
