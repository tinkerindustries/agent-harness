package deepseek

import (
	"encoding/json"
	"strings"
)

// ToolCallAssembler reconstructs complete tool calls from streamed deltas.
// Deltas are keyed by index; id, type, and name arrive once on the opening
// frame for that index, and arguments arrive as fragments that must be
// concatenated in arrival order, mid-token and mid-string
// (docs/OBSERVED.md).
type ToolCallAssembler struct {
	order []int
	calls map[int]*assembledCall
}

type assembledCall struct {
	id, typ, name string
	arguments     []byte
}

// AssembledToolCall is a tool call with its arguments fully concatenated.
// Arguments is the raw text the model produced; it is not guaranteed to be
// valid JSON and must be validated before use.
type AssembledToolCall struct {
	ID        string
	Type      string
	Name      string
	Arguments string
}

// NewToolCallAssembler returns an empty assembler.
func NewToolCallAssembler() *ToolCallAssembler {
	return &ToolCallAssembler{calls: make(map[int]*assembledCall)}
}

// Add folds one streamed delta into the assembler.
func (a *ToolCallAssembler) Add(d ToolCallDelta) {
	c, ok := a.calls[d.Index]
	if !ok {
		c = &assembledCall{}
		a.calls[d.Index] = c
		a.order = append(a.order, d.Index)
	}
	if d.ID != "" {
		c.id = d.ID
	}
	if d.Type != "" {
		c.typ = d.Type
	}
	if d.Function.Name != "" {
		c.name = d.Function.Name
	}
	c.arguments = append(c.arguments, d.Function.Arguments...)
}

// Finalize returns the assembled calls in the order their index first
// appeared.
func (a *ToolCallAssembler) Finalize() []AssembledToolCall {
	out := make([]AssembledToolCall, 0, len(a.order))
	for _, idx := range a.order {
		c := a.calls[idx]
		out = append(out, AssembledToolCall{
			ID:        c.id,
			Type:      c.typ,
			Name:      c.name,
			Arguments: string(c.arguments),
		})
	}
	return out
}

// RepairArguments corrects a single misplaced brace in assembled tool-call
// arguments. It returns the repaired text and true when it changed
// something, and args unchanged and false otherwise (docs/OBSERVED.md).
//
// Two shapes are repaired, both unambiguous — there is exactly one object
// the bytes can have meant:
//
//   - one closing brace short at the end, which has one completion; and
//   - one closing brace too many after an otherwise complete object, which
//     has one deletion.
//
// A third shape is deliberately not repaired, because it is ambiguous. When
// the model closes an object early and carries on with another key —
// `{"result":{…},"error":""},"summary":"…"}` — deleting either brace yields
// valid JSON, and the two differ in whether "error" lands inside result or
// beside it. Only the tool's schema knows which was meant, and this package
// does not have it. Guessing would risk the worse failure: not a rejected
// call but an accepted one carrying a shape the model never wrote. The
// executor's schema error names the missing property, which is the better
// answer anyway.
//
// finishReason gates the repair. Only a clean stop is repaired: on
// FinishLength the arguments were cut off wherever the budget ran out, and
// a payload that happens to be one brace short of parsing is a truncated
// payload, not a misplaced brace. Balancing it would turn a partial result
// into a complete-looking one and hand it to a tool as though the model had
// meant it.
//
// What this buys is a better error rather than a saved sub-turn: a call
// that fails to parse is rejected with a position in a byte stream the
// model cannot see, while one that parses is rejected — if it still is —
// by name and property. Anything not repaired here is left for the
// executor to reject, which is the behaviour this supplements and remains
// the fallback.
func RepairArguments(finishReason, args string) (string, bool) {
	if finishReason != FinishToolCalls {
		return args, false
	}
	s := strings.TrimSpace(args)
	if s == "" || isJSONObject(s) {
		return args, false
	}
	for _, candidate := range []string{closeOneObject(s), dropDoubledClose(s)} {
		if candidate != "" && isJSONObject(candidate) {
			return candidate, true
		}
	}
	return args, false
}

// closeOneObject repairs arguments that end one closing brace short, by
// appending it. It returns "" unless exactly one delimiter is open at the
// end and it is a brace, so a payload missing two levels — far more likely
// to be truncation than a slip — is not guessed at.
func closeOneObject(s string) string {
	open, inString, ok := openDelimiters(s)
	if !ok || inString || len(open) != 1 || open[0] != '{' {
		return ""
	}
	return s + "}"
}

// dropDoubledClose repairs arguments that carry one closing brace too many,
// by returning just the complete object in front of it. The trailing brace
// has to be all that follows: anything else after a complete object is a
// key the model meant to put somewhere, and where it belongs is the
// ambiguity RepairArguments declines to resolve.
func dropDoubledClose(s string) string {
	dec := json.NewDecoder(strings.NewReader(s))
	var leading any
	if err := dec.Decode(&leading); err != nil {
		return ""
	}
	end := int(dec.InputOffset())
	if end <= 0 || end > len(s) || strings.TrimSpace(s[end:]) != "}" {
		return ""
	}
	return s[:end]
}

// openDelimiters returns the braces and brackets left unclosed at the end
// of s, outermost first, and whether s ends inside a string literal.
// Delimiters inside strings are not structure and are skipped, as is any
// character escaped by a backslash. ok is false if a closer appears with no
// matching opener or closes the wrong kind, which means the text is
// malformed in a way this file does not repair.
func openDelimiters(s string) (open []byte, inString bool, ok bool) {
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{', '[':
			open = append(open, c)
		case '}', ']':
			want := byte('{')
			if c == ']' {
				want = '['
			}
			if len(open) == 0 || open[len(open)-1] != want {
				return nil, false, false
			}
			open = open[:len(open)-1]
		}
	}
	return open, inString, true
}

// isJSONObject reports whether s is a complete JSON object. Tool arguments
// are always objects, so a repair that produces anything else — an array, a
// bare string, null — has not reconstructed what the model meant.
func isJSONObject(s string) bool {
	var obj map[string]json.RawMessage
	return json.Unmarshal([]byte(s), &obj) == nil && obj != nil
}
