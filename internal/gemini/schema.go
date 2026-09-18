package gemini

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// schema.go: making a tool's JSON Schema something the Interactions API will
// accept.
//
// Google's surface is more tolerant than its Schema proto suggests. Measured
// against the live API, it accepts `$schema`, `$ref` with `$defs`,
// `additionalProperties`, `const`, `oneOf`, `allOf`, `anyOf`, `default`,
// `format`, `examples`, `prefixItems`, a type union with "null", and
// keywords it has never heard of. One construct it refuses:
//
//	"items": [{"type": "number"}, {"type": "number"}]
//
// JSON Schema's tuple form, where `items` is an array of per-position
// schemas rather than one schema for every element. Google answers
//
//	400 invalid_request: Invalid JSON payload: syntax error in request body.
//
// which names no tool, no field and no schema — and because one bad
// declaration invalidates the whole payload, every tool in the request goes
// down with it and the session dies on its first request. A CAD
// session died this way for one `z.tuple([number, number, number])` in a
// single tool, and finding it meant diffing the tool array against a session
// that still worked.
//
// Nothing else is stripped. Rewriting constructs Google accepts would change
// the frozen prefix for no gain, and a schema says what its author meant it
// to say.
//
// LowerToolSchemas is called once, where the tool array is resolved and
// frozen (internal/session/lifecycle.go), never per request. The bytes it
// produces are what the row stores and what every request carries, so
// docs/DESIGN.md §3.2's rule — the request path never round-trips a schema
// through a map that could reorder its keys — still holds: the reorder
// happens once, before the prefix is frozen, and only on the nodes that
// actually contained a tuple.

// LowerToolSchemas returns tools with every tuple-form `items` rewritten
// into the single-schema form the Interactions API accepts. A tool whose
// schema contains no tuple is returned with its parameter bytes untouched,
// so the overwhelming majority of a tool array is byte-identical to what the
// client declared.
func LowerToolSchemas(tools []wire.Tool) ([]wire.Tool, error) {
	if len(tools) == 0 {
		return tools, nil
	}
	out := make([]wire.Tool, len(tools))
	copy(out, tools)
	for i, t := range out {
		if len(t.Function.Parameters) == 0 {
			continue
		}
		lowered, changed, err := lowerNode(t.Function.Parameters)
		if err != nil {
			return nil, fmt.Errorf("gemini: lower schema for tool %q: %w", t.Function.Name, err)
		}
		if changed {
			out[i].Function.Parameters = lowered
		}
	}
	return out, nil
}

// lowerNode walks one schema node. It reports whether anything beneath it
// changed, and returns raw untouched when nothing did — so a node that
// contains no tuple keeps the exact bytes, key order included, that its
// author wrote. Only a node on the path to a real rewrite is re-marshalled.
func lowerNode(raw json.RawMessage) (json.RawMessage, bool, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return raw, false, nil
	}
	switch trimmed[0] {
	case '{':
		return lowerObject(raw)
	case '[':
		return lowerArray(raw)
	default:
		return raw, false, nil
	}
}

func lowerObject(raw json.RawMessage) (json.RawMessage, bool, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, false, err
	}
	changed := false
	for key, value := range obj {
		// `items` holding an array is the tuple form. Every other key —
		// including `prefixItems`, which Google accepts — is walked as an
		// ordinary node.
		if key == "items" && isJSONArray(value) {
			lowered, count, err := lowerTupleItems(value)
			if err != nil {
				return nil, false, err
			}
			obj["items"] = lowered
			// The bound the tuple expressed by its length. Only supplied
			// when the author did not already say, so an explicit bound is
			// never overwritten.
			if _, ok := obj["minItems"]; !ok {
				obj["minItems"] = json.RawMessage(fmt.Sprintf("%d", count))
			}
			if _, ok := obj["maxItems"]; !ok {
				obj["maxItems"] = json.RawMessage(fmt.Sprintf("%d", count))
			}
			changed = true
			continue
		}
		lowered, subChanged, err := lowerNode(value)
		if err != nil {
			return nil, false, err
		}
		if subChanged {
			obj[key] = lowered
			changed = true
		}
	}
	if !changed {
		return raw, false, nil
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

func lowerArray(raw json.RawMessage) (json.RawMessage, bool, error) {
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, false, err
	}
	changed := false
	for i, value := range arr {
		lowered, subChanged, err := lowerNode(value)
		if err != nil {
			return nil, false, err
		}
		if subChanged {
			arr[i] = lowered
			changed = true
		}
	}
	if !changed {
		return raw, false, nil
	}
	out, err := json.Marshal(arr)
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

// lowerTupleItems turns the tuple's member schemas into the one schema every
// element is held to, and reports how many members there were.
//
// Members that all say the same thing — which is every tuple the harness has
// actually been given, since a tuple of coordinates is a tuple of numbers —
// collapse to that one schema, and the rewrite loses nothing but the
// per-position addressing, which the length bounds restore. Members that
// differ become an `anyOf` over the distinct ones: weaker than the tuple,
// because it no longer says which member belongs in which position, but it
// is the closest this surface can express and it keeps every member's own
// constraints. Order of first appearance is kept, so the result is stable.
func lowerTupleItems(raw json.RawMessage) (json.RawMessage, int, error) {
	var members []json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return nil, 0, err
	}
	if len(members) == 0 {
		return json.RawMessage(`{}`), 0, nil
	}

	distinct := make([]json.RawMessage, 0, len(members))
	seen := make([]string, 0, len(members))
	for _, m := range members {
		lowered, _, err := lowerNode(m)
		if err != nil {
			return nil, 0, err
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, lowered); err != nil {
			return nil, 0, err
		}
		key := compact.String()
		if containsString(seen, key) {
			continue
		}
		seen = append(seen, key)
		distinct = append(distinct, lowered)
	}

	if len(distinct) == 1 {
		return distinct[0], len(members), nil
	}
	out, err := json.Marshal(map[string]json.RawMessage{"anyOf": mustMarshalArray(distinct)})
	if err != nil {
		return nil, 0, err
	}
	return out, len(members), nil
}

func mustMarshalArray(items []json.RawMessage) json.RawMessage {
	out, err := json.Marshal(items)
	if err != nil {
		// Every element came out of json.Marshal or json.Compact above, so
		// this cannot fail on well-formed input.
		return json.RawMessage(`[]`)
	}
	return out
}

func isJSONArray(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '['
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
