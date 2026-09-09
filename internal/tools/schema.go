package tools

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ValidateAgainstSchema checks data against schema, a JSON Schema document.
// It covers the subset a Complete result_schema realistically needs: type,
// properties/required/additionalProperties, items, enum, and the numeric
// and string bounds in docs/sources's tool_calls.md strict-mode subset. It
// is not a full draft implementation — no $ref, no allOf/oneOf, no format
// validators — because Complete's schemas are supplied by the request
// and are expected to be plain data shapes, not general-purpose contracts.
func ValidateAgainstSchema(schema, data json.RawMessage) []string {
	if len(schema) == 0 {
		return nil
	}
	var s map[string]any
	if err := json.Unmarshal(schema, &s); err != nil {
		return []string{fmt.Sprintf("result_schema is not valid JSON: %v", err)}
	}
	var v any
	if len(data) == 0 {
		data = []byte("null")
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return []string{fmt.Sprintf("result is not valid JSON: %v", err)}
	}
	var errs []string
	validate(s, v, "result", &errs)
	sort.Strings(errs)
	return errs
}

func validate(schema map[string]any, v any, path string, errs *[]string) {
	if enum, ok := schema["enum"].([]any); ok {
		if !containsValue(enum, v) {
			*errs = append(*errs, fmt.Sprintf("%s: must be one of %v", path, enum))
			return
		}
	}

	t, _ := schema["type"].(string)
	switch t {
	case "object":
		obj, ok := v.(map[string]any)
		if !ok {
			*errs = append(*errs, fmt.Sprintf("%s: expected object, got %s", path, jsonTypeName(v)))
			return
		}
		for _, req := range asStringSlice(schema["required"]) {
			if _, present := obj[req]; !present {
				*errs = append(*errs, fmt.Sprintf("%s: missing required property %q", path, req))
			}
		}
		props, _ := schema["properties"].(map[string]any)
		if additional, ok := schema["additionalProperties"].(bool); ok && !additional {
			for k := range obj {
				if _, defined := props[k]; !defined {
					*errs = append(*errs, fmt.Sprintf("%s: unexpected property %q", path, k))
				}
			}
		}
		for k, sub := range props {
			subSchema, ok := sub.(map[string]any)
			if !ok {
				continue
			}
			if val, present := obj[k]; present {
				validate(subSchema, val, path+"."+k, errs)
			}
		}

	case "array":
		arr, ok := v.([]any)
		if !ok {
			*errs = append(*errs, fmt.Sprintf("%s: expected array, got %s", path, jsonTypeName(v)))
			return
		}
		itemSchema, _ := schema["items"].(map[string]any)
		if itemSchema != nil {
			for i, item := range arr {
				validate(itemSchema, item, fmt.Sprintf("%s[%d]", path, i), errs)
			}
		}

	case "string":
		if _, ok := v.(string); !ok {
			*errs = append(*errs, fmt.Sprintf("%s: expected string, got %s", path, jsonTypeName(v)))
		}

	case "boolean":
		if _, ok := v.(bool); !ok {
			*errs = append(*errs, fmt.Sprintf("%s: expected boolean, got %s", path, jsonTypeName(v)))
		}

	case "number", "integer":
		n, ok := v.(float64)
		if !ok {
			*errs = append(*errs, fmt.Sprintf("%s: expected %s, got %s", path, t, jsonTypeName(v)))
			return
		}
		if t == "integer" && n != float64(int64(n)) {
			*errs = append(*errs, fmt.Sprintf("%s: expected integer, got a fractional number", path))
		}
		if min, ok := schema["minimum"].(float64); ok && n < min {
			*errs = append(*errs, fmt.Sprintf("%s: must be >= %v", path, min))
		}
		if max, ok := schema["maximum"].(float64); ok && n > max {
			*errs = append(*errs, fmt.Sprintf("%s: must be <= %v", path, max))
		}

	case "":
		// No type constraint: any value is acceptable once enum (already
		// checked above) passes.
	}
}

func containsValue(options []any, v any) bool {
	vb, err := json.Marshal(v)
	if err != nil {
		return false
	}
	for _, o := range options {
		ob, err := json.Marshal(o)
		if err == nil && string(ob) == string(vb) {
			return true
		}
	}
	return false
}

func asStringSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func jsonTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return strings.ToLower(fmt.Sprintf("%T", v))
	}
}
