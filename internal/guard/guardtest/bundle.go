package guardtest

import (
	"bytes"
	"encoding/json"
)

// Model ids and values of the invented bundle BundleJSON returns.
const (
	Stage1Model = "invented-stage1-model"
	Stage2Model = "invented-stage2-model"
	Cutoff      = 0.3
	// Question is the name of the invented bundle's one stage 1 question.
	Question = "gate_check"
)

// BundleJSON returns an invented policy bundle that uses every part of the
// format: each macro, several patterns under one rule name, an exception,
// default branches, and both stages. Nothing in it is real policy. Each edit
// changes the decoded bundle before it is encoded.
func BundleJSON(edits ...func(map[string]any)) []byte {
	b := map[string]any{
		"schema_version": 1,
		"cutoff":         Cutoff,
		"stage1": map[string]any{
			"model":        Stage1Model,
			"state_fields": []any{"working_directory", "branch", "recent_commands", "proposed_command", "scripts_run"},
			"questions": json.RawMessage(`{"` + Question + `":{"type":"choice","instructions":"Invented instructions with <angle> & ampersand.",` +
				`"criteria":{"allow":"Invented allow text.","block":"Invented block text."}}}`),
		},
		"stage2": map[string]any{
			"model":        Stage2Model,
			"effort":       "low",
			"max_tokens":   1234,
			"state_fields": []any{"working_directory", "branch", "recent_commands", "proposed_command", "scripts_run", "package_scripts"},
			"system":       "Invented system prompt with <angle> & ampersand.",
			"schema": json.RawMessage(`{"type":"object","properties":{"reason":{"type":"string"},"decision":{"type":"string","enum":["allow","block"]}},` +
				`"required":["reason","decision"],"additionalProperties":false}`),
		},
		"rules": map[string]any{
			"macros": map[string]any{
				"space_chars": `\t-\r \x{a0}`,
				"word_chars":  `\p{L}\p{N}_`,
				"space":       `[${space_chars}]`,
				"word":        `[${word_chars}]`,
				"word_end":    `(?:$|[^${word_chars}])`,
			},
			"table": []any{
				map[string]any{"name": "deploy_prod", "patterns": []any{
					map[string]any{"match": `deploy${space}+prod${word_end}`},
					map[string]any{"match": `ship${space}+it${word_end}`},
				}},
				map[string]any{"name": "elevate", "patterns": []any{
					map[string]any{"match": `elevate${space}`},
				}},
				map[string]any{"name": "remote_copy", "patterns": []any{
					map[string]any{"match": `beamto${space}[^\n;&|]*${space}[${word_chars}.@-]+:`},
					map[string]any{"match": `relay${space}+${word}+@`},
				}},
				map[string]any{"name": "timer", "patterns": []any{
					map[string]any{"match": `settimer${space}+`, "unless_followed_by": `$|-q${word_end}`},
				}},
			},
			"default_branches": []any{"trunk", "stable"},
		},
	}
	for _, edit := range edits {
		edit(b)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(b); err != nil {
		panic(err)
	}
	return buf.Bytes()
}
