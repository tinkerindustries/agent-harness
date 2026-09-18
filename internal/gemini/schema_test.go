package gemini

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// hasTupleItems reports whether any node in raw carries an `items` holding
// an array — the one construct the Interactions API refuses.
func hasTupleItems(t *testing.T, raw json.RawMessage) bool {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err == nil {
		for key, value := range obj {
			if key == "items" && isJSONArray(value) {
				return true
			}
			if hasTupleItems(t, value) {
				return true
			}
		}
		return false
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err == nil {
		for _, value := range arr {
			if hasTupleItems(t, value) {
				return true
			}
		}
	}
	return false
}

func toolWith(name string, params string) wire.Tool {
	return wire.Tool{Type: "function", Function: wire.ToolFunction{
		Name: name, Description: "d", Parameters: json.RawMessage(params),
	}}
}

func loweredParams(t *testing.T, params string) string {
	t.Helper()
	out, err := LowerToolSchemas([]wire.Tool{toolWith("T", params)})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, out[0].Function.Parameters); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// TestLowerCollapsesHomogeneousTuple is the shape every tuple the harness
// has actually been given takes: a fixed-length run of one type, which is
// what z.tuple([number, number, number]) renders to. The length the tuple
// expressed survives as minItems/maxItems.
func TestLowerCollapsesHomogeneousTuple(t *testing.T) {
	got := loweredParams(t, `{"type":"object","properties":{"target":{"type":"array","items":[{"type":"number"},{"type":"number"},{"type":"number"}]}}}`)
	want := `{"properties":{"target":{"items":{"type":"number"},"maxItems":3,"minItems":3,"type":"array"}},"type":"object"}`
	if got != want {
		t.Fatalf("lowered schema:\n got: %s\nwant: %s", got, want)
	}
}

// TestLowerHeterogeneousTupleBecomesAnyOf covers a tuple whose members
// disagree. There is no way to say "a string then a number" on this surface,
// so the rewrite keeps both members' constraints under anyOf and lets the
// length bounds carry the arity. It would fail if a member were dropped.
func TestLowerHeterogeneousTupleBecomesAnyOf(t *testing.T) {
	got := loweredParams(t, `{"type":"object","properties":{"p":{"type":"array","items":[{"type":"string"},{"type":"number"}]}}}`)
	want := `{"properties":{"p":{"items":{"anyOf":[{"type":"string"},{"type":"number"}]},"maxItems":2,"minItems":2,"type":"array"}},"type":"object"}`
	if got != want {
		t.Fatalf("lowered schema:\n got: %s\nwant: %s", got, want)
	}
}

// TestLowerKeepsAuthorsBoundsAndNesting pins two things at once: an explicit
// minItems the author wrote is never overwritten by the tuple's length, and
// a tuple buried below other keywords is still found.
func TestLowerKeepsAuthorsBoundsAndNesting(t *testing.T) {
	got := loweredParams(t, `{"type":"object","properties":{"outer":{"type":"object","properties":{"inner":{"type":"array","minItems":1,"items":[{"type":"number"},{"type":"number"}]}}}}}`)
	want := `{"properties":{"outer":{"properties":{"inner":{"items":{"type":"number"},"maxItems":2,"minItems":1,"type":"array"}},"type":"object"}},"type":"object"}`
	if got != want {
		t.Fatalf("lowered schema:\n got: %s\nwant: %s", got, want)
	}
}

// TestLowerLeavesCleanSchemasByteIdentical is why the pass walks with
// json.RawMessage instead of decoding everything into maps. A schema with no
// tuple in it must come back as the exact bytes its author wrote, key order
// included: these bytes become the session's frozen prefix, and rewriting
// them for no reason would be a cache miss and a schema nobody wrote.
//
// The keywords here are the ones a broader "strip anything outside Google's
// Schema proto" pass would have removed. Measured against the live API, the
// Interactions surface accepts every one of them, so removing them would
// cost meaning and buy nothing.
func TestLowerLeavesCleanSchemasByteIdentical(t *testing.T) {
	params := `{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","additionalProperties":false,"properties":{"zebra":{"type":"string","format":"date-time","default":"x"},"alpha":{"const":"k"},"list":{"type":"array","items":{"type":"number"}},"choice":{"oneOf":[{"type":"string"},{"type":"number"}]}},"required":["zebra"]}`
	out, err := LowerToolSchemas([]wire.Tool{toolWith("T", params)})
	if err != nil {
		t.Fatal(err)
	}
	if string(out[0].Function.Parameters) != params {
		t.Fatalf("a schema with no tuple was rewritten:\n got: %s\nwant: %s", out[0].Function.Parameters, params)
	}
}

// TestLowerCADToolsRemovesEveryTuple runs the pass over the tool array
// that actually broke: the tools a CAD session sent to
// gemini-3.8-flash, which the Interactions API refused wholesale over one
// z.tuple in design_render's `target`. The unlowered array is checked to
// still carry a tuple, so the fixture cannot rot into proving nothing.
//
// Verified against the live API while this was written: the array as
// checked in answers 400 "Invalid JSON payload: syntax error in request
// body", and the lowered array answers 200.
func TestLowerCADToolsRemovesEveryTuple(t *testing.T) {
	raw, err := os.ReadFile("testdata/cad-tools.json")
	if err != nil {
		t.Fatal(err)
	}
	var tools []wire.Tool
	if err := json.Unmarshal(raw, &tools); err != nil {
		t.Fatal(err)
	}
	if len(tools) == 0 {
		t.Fatal("the fixture decoded to no tools")
	}

	offenders := 0
	for _, tl := range tools {
		if hasTupleItems(t, tl.Function.Parameters) {
			offenders++
			t.Logf("unlowered tuple in %s", tl.Function.Name)
		}
	}
	if offenders == 0 {
		t.Fatal("the fixture carries no tuple-form items, so it proves nothing")
	}

	lowered, err := LowerToolSchemas(tools)
	if err != nil {
		t.Fatal(err)
	}
	if len(lowered) != len(tools) {
		t.Fatalf("lowering returned %d tools, want %d", len(lowered), len(tools))
	}
	for _, tl := range lowered {
		if hasTupleItems(t, tl.Function.Parameters) {
			t.Errorf("tool %q still carries a tuple-form items after lowering", tl.Function.Name)
		}
	}

	// Every tool that had no tuple keeps its exact bytes, so the frozen
	// prefix moves only where it had to.
	untouched := 0
	for i, tl := range tools {
		if !hasTupleItems(t, tl.Function.Parameters) {
			if !bytes.Equal(tl.Function.Parameters, lowered[i].Function.Parameters) {
				t.Errorf("tool %q had no tuple but its bytes changed", tl.Function.Name)
			}
			untouched++
		}
	}
	t.Logf("%d tools carried a tuple, %d left byte-identical", offenders, untouched)
}
