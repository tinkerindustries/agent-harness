// Package toolvocab is how one tool array names the harness's tools to the
// model. The harness has one vocabulary of its own — the lower_snake_case
// names and argument spellings internal/tools dispatches on, every event in
// the log carries, the permission policy and deny patterns match, and every
// parent-facing frame shows. Most arrays offer the model exactly those. A
// Claude model is offered Claude Code's own tools instead (Read, Bash with
// timeout, TaskOutput with task_id, TaskCreate one task at a time, Agent):
// the vocabulary it was trained to use, rather than the DeepSeek harness's
// one the canonical names were aligned with
// (docs/plans/archive/deepseek-tool-vocabulary.md, docs/TOOLS.md "Claude's
// array").
//
// The translation happens at the model boundary and nowhere else. A call the
// model makes is turned into the canonical name and arguments before it is
// committed to the log (FromModel), so everything downstream of the request
// sees one vocabulary; history rendered back into a request is turned the
// other way (ItemsToModel) so the model reads its own calls in the names it
// was offered. A Vocabulary is a pure function of the tool array it was
// built from (Of), which is frozen on the session row, so a resumed session
// translates exactly as it did when it started — and a session whose row
// carries the canonical names translates nothing at all.
//
// Depends on: internal/wire.
package toolvocab

import (
	"bytes"
	"encoding/json"

	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// entry is one tool whose name, and possibly arguments, differ between the
// harness's canonical vocabulary and the one the model is offered.
type entry struct {
	canonical string
	exposed   string
	// renames pairs each argument's canonical spelling with the model's,
	// {canonical, exposed}. Applied left to right in the direction the
	// arguments are travelling.
	renames [][2]string
	// fromModel finishes a call's arguments after the renames, for what a
	// rename cannot say: a default the model's schema implies, or a
	// reshaping. nil for none.
	fromModel func(args map[string]json.RawMessage) map[string]json.RawMessage
	// toModel undoes fromModel's reshaping when history is rendered back
	// into a request. nil for none.
	toModel func(args map[string]json.RawMessage) map[string]json.RawMessage
}

// claudeCode is Claude Code's tool vocabulary, one entry per tool the
// harness offers under a Claude Code name. A tool Claude Code has no
// counterpart for is not offered to a Claude model at all
// (internal/tools.definitionsClaude), so it has no entry here.
var claudeCode = []entry{
	{canonical: "read", exposed: "Read"},
	{canonical: "write", exposed: "Write"},
	{canonical: "edit", exposed: "Edit"},
	{canonical: "bash", exposed: "Bash", renames: [][2]string{{"timeoutMs", "timeout"}}},
	{
		// TaskOutput blocks by default; job_output does not. An omitted
		// block is Claude Code's default, so it becomes an explicit wait.
		canonical: "job_output", exposed: "TaskOutput",
		renames: [][2]string{{"job_id", "task_id"}, {"wait", "block"}, {"timeoutMs", "timeout"}},
		fromModel: func(args map[string]json.RawMessage) map[string]json.RawMessage {
			if _, ok := args["wait"]; !ok {
				args["wait"] = json.RawMessage("true")
			}
			return args
		},
	},
	{canonical: "job_kill", exposed: "TaskStop", renames: [][2]string{{"job_id", "task_id"}}},
	{canonical: "glob", exposed: "Glob"},
	{canonical: "grep", exposed: "Grep"},
	{
		// Claude Code's TaskCreate adds one task per call; task_create takes
		// an array. activeForm is optional in Claude Code's schema and
		// required by task_create, so an omitted one falls back to the
		// subject.
		canonical: "task_create", exposed: "TaskCreate",
		fromModel: func(args map[string]json.RawMessage) map[string]json.RawMessage {
			if _, ok := args["activeForm"]; !ok {
				if subject, ok := args["subject"]; ok {
					args["activeForm"] = subject
				}
			}
			item, err := json.Marshal(args)
			if err != nil {
				return args
			}
			return map[string]json.RawMessage{"tasks": json.RawMessage("[" + string(item) + "]")}
		},
		toModel: func(args map[string]json.RawMessage) map[string]json.RawMessage {
			var tasks []map[string]json.RawMessage
			if json.Unmarshal(args["tasks"], &tasks) != nil || len(tasks) != 1 || len(args) != 1 {
				return args
			}
			return tasks[0]
		},
	},
	{canonical: "task_get", exposed: "TaskGet"},
	{canonical: "task_list", exposed: "TaskList"},
	{canonical: "task_update", exposed: "TaskUpdate"},
	{
		// Agent's subagent_type is optional in Claude Code's schema and
		// defaults to its general-purpose agent; task requires one.
		canonical: "task", exposed: "Agent",
		fromModel: func(args map[string]json.RawMessage) map[string]json.RawMessage {
			if t, ok := args["subagent_type"]; !ok || string(t) == `""` {
				args["subagent_type"] = json.RawMessage(`"general-purpose"`)
			}
			return args
		},
	},
}

var claudeCodeByExposed = func() map[string]*entry {
	m := make(map[string]*entry, len(claudeCode))
	for i := range claudeCode {
		m[claudeCode[i].exposed] = &claudeCode[i]
	}
	return m
}()

// ClaudeCodeName returns the name Claude Code gives the canonical tool, and
// whether it has one.
func ClaudeCodeName(canonical string) (string, bool) {
	for i := range claudeCode {
		if claudeCode[i].canonical == canonical {
			return claudeCode[i].exposed, true
		}
	}
	return "", false
}

// Vocabulary maps one tool array's names onto the harness's canonical ones.
// The zero value is the identity: every name and argument passes through
// unchanged and every canonical tool counts as offered, which is what a
// caller that never built one (a test, a direct CLI caller) means.
type Vocabulary struct {
	byExposed   map[string]*entry
	byCanonical map[string]*entry
	// offered is the canonical name of every tool the array carries; nil
	// for the zero value, which offers everything.
	offered map[string]bool
}

// Of builds the vocabulary of array: every tool in it whose name is a Claude
// Code one is translated, every other passes through. An array is either one
// vocabulary or the other in practice — internal/tools builds each frozen
// array whole — but resolving per tool keeps a variant's smaller array (one
// missing Bash, say) translating the tools it does carry.
func Of(array []wire.Tool) Vocabulary {
	v := Vocabulary{offered: make(map[string]bool, len(array))}
	for _, t := range array {
		name := t.Function.Name
		if e, ok := claudeCodeByExposed[name]; ok {
			if v.byExposed == nil {
				v.byExposed = map[string]*entry{}
				v.byCanonical = map[string]*entry{}
			}
			v.byExposed[name] = e
			v.byCanonical[e.canonical] = e
			name = e.canonical
		}
		v.offered[name] = true
	}
	return v
}

// OfSchema is Of for a tool array still in its serialised form, the session
// row's tool_schema. A schema that does not parse yields the identity.
func OfSchema(raw json.RawMessage) Vocabulary {
	if len(bytes.TrimSpace(raw)) == 0 {
		return Vocabulary{}
	}
	var array []wire.Tool
	if json.Unmarshal(raw, &array) != nil {
		return Vocabulary{}
	}
	return Of(array)
}

// ClaudeCode reports whether the array offers any tool under its Claude Code
// name.
func (v Vocabulary) ClaudeCode() bool { return len(v.byExposed) > 0 }

// Offers reports whether the array carries the canonical tool, under
// whichever name. The zero value offers everything.
func (v Vocabulary) Offers(canonical string) bool {
	return v.offered == nil || v.offered[canonical]
}

// Exposed returns the name the model is offered the canonical tool under.
func (v Vocabulary) Exposed(canonical string) string {
	if e, ok := v.byCanonical[canonical]; ok {
		return e.exposed
	}
	return canonical
}

// Canonical returns the harness's name for a tool the model called by name.
// A name the vocabulary does not know — a canonical one, an MCP tool, one
// the model made up — is returned unchanged.
func (v Vocabulary) Canonical(name string) string {
	if e, ok := v.byExposed[name]; ok {
		return e.canonical
	}
	return name
}

// ExposedArg returns the name the model is offered a canonical tool's
// canonical argument under.
func (v Vocabulary) ExposedArg(canonicalTool, canonicalArg string) string {
	if e, ok := v.byCanonical[canonicalTool]; ok {
		for _, r := range e.renames {
			if r[0] == canonicalArg {
				return r[1]
			}
		}
	}
	return canonicalArg
}

// FromModel turns a call the model made into the harness's canonical name
// and arguments. Arguments that do not parse as a JSON object pass through
// unchanged, so the tool reports them as invalid in its own words; a call
// whose tool needs no translation keeps its exact bytes.
func (v Vocabulary) FromModel(name, arguments string) (string, string) {
	e, ok := v.byExposed[name]
	if !ok {
		return name, arguments
	}
	return e.canonical, rewrite(arguments, e.renames, 1, 0, e.fromModel)
}

// ToModel turns a canonical call from the log back into the name and
// arguments the model is offered, for history rendered into a request.
func (v Vocabulary) ToModel(name, arguments string) (string, string) {
	e, ok := v.byCanonical[name]
	if !ok {
		return name, arguments
	}
	return e.exposed, rewrite(arguments, e.renames, 0, 1, e.toModel)
}

// ItemsToModel returns items with every function_call item renamed into the
// model's vocabulary. items itself is never modified: a slice that needs no
// change is returned as it is, and one that does is copied first, because
// the caller's slice is the fold's output and is read again after the
// request is built.
func (v Vocabulary) ItemsToModel(items []wire.Item) []wire.Item {
	if !v.ClaudeCode() {
		return items
	}
	var out []wire.Item
	for i, item := range items {
		if item.Type != wire.ItemFunctionCall {
			continue
		}
		name, args := v.ToModel(item.Name, item.Arguments)
		if name == item.Name && args == item.Arguments {
			continue
		}
		if out == nil {
			out = append([]wire.Item(nil), items...)
		}
		out[i].Name, out[i].Arguments = name, args
	}
	if out == nil {
		return items
	}
	return out
}

// rewrite renames arguments from renames[i][from] to renames[i][to] and then
// applies finish. Arguments that need neither keep their exact bytes. Empty
// arguments read as an empty object, the way every tool handler reads them.
func rewrite(arguments string, renames [][2]string, from, to int,
	finish func(map[string]json.RawMessage) map[string]json.RawMessage) string {

	if len(renames) == 0 && finish == nil {
		return arguments
	}
	raw := bytes.TrimSpace([]byte(arguments))
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	var args map[string]json.RawMessage
	if json.Unmarshal(raw, &args) != nil || args == nil {
		return arguments
	}
	for _, r := range renames {
		if val, ok := args[r[from]]; ok {
			delete(args, r[from])
			args[r[to]] = val
		}
	}
	if finish != nil {
		args = finish(args)
	}
	out, err := json.Marshal(args)
	if err != nil {
		return arguments
	}
	return string(out)
}
