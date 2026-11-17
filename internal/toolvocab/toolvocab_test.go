package toolvocab

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

func array(names ...string) []wire.Tool {
	out := make([]wire.Tool, len(names))
	for i, n := range names {
		out[i] = wire.Tool{Type: "function", Function: wire.ToolFunction{Name: n}}
	}
	return out
}

// claudeArray is every Claude Code name the vocabulary knows.
func claudeArray() []wire.Tool {
	names := make([]string, len(claudeCode))
	for i, e := range claudeCode {
		names[i] = e.exposed
	}
	return array(names...)
}

// sameJSON reports whether two JSON texts decode to the same value, so a
// test is about the arguments and not about the order Marshal wrote them in.
func sameJSON(t *testing.T, got, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("got is not JSON: %v: %s", err, got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("want is not JSON: %v: %s", err, want)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("arguments differ:\ngot:  %s\nwant: %s", got, want)
	}
}

// TestFromModelTranslatesClaudeCodeCalls pins what each Claude Code call
// becomes: the canonical name, and arguments the canonical handler reads.
func TestFromModelTranslatesClaudeCodeCalls(t *testing.T) {
	v := Of(claudeArray())
	cases := []struct {
		name, args         string
		wantName, wantArgs string
	}{
		{"Read", `{"file_path":"a.go","limit":5}`, "read", `{"file_path":"a.go","limit":5}`},
		{"Bash", `{"command":"go test ./...","timeout":600000}`, "bash", `{"command":"go test ./...","timeoutMs":600000}`},
		{"TaskOutput", `{"task_id":"bash_1"}`, "job_output", `{"job_id":"bash_1","wait":true}`},
		{"TaskOutput", `{"task_id":"bash_1","block":false,"timeout":5000}`, "job_output", `{"job_id":"bash_1","wait":false,"timeoutMs":5000}`},
		{"TaskStop", `{"task_id":"bash_2"}`, "job_kill", `{"job_id":"bash_2"}`},
		{"TaskCreate", `{"subject":"Run tests","description":"go test","activeForm":"Running tests"}`,
			"task_create", `{"tasks":[{"subject":"Run tests","description":"go test","activeForm":"Running tests"}]}`},
		{"TaskCreate", `{"subject":"Run tests","description":"go test"}`,
			"task_create", `{"tasks":[{"subject":"Run tests","description":"go test","activeForm":"Run tests"}]}`},
		{"TaskUpdate", `{"taskId":"1","status":"completed"}`, "task_update", `{"taskId":"1","status":"completed"}`},
		{"Agent", `{"description":"find it","prompt":"look"}`, "task", `{"description":"find it","prompt":"look","subagent_type":"general-purpose"}`},
		{"Agent", `{"description":"find it","prompt":"look","subagent_type":"Explore"}`, "task", `{"description":"find it","prompt":"look","subagent_type":"Explore"}`},
	}
	for _, tc := range cases {
		name, args := v.FromModel(tc.name, tc.args)
		if name != tc.wantName {
			t.Errorf("FromModel(%s) name = %q, want %q", tc.name, name, tc.wantName)
			continue
		}
		sameJSON(t, args, tc.wantArgs)
	}
}

// TestFromModelLeavesWhatItDoesNotKnow pins the pass-throughs: a canonical
// name (an older Claude session's frozen array offered those), an MCP tool,
// a made-up name, and arguments that are not a JSON object all reach the
// executor exactly as the model sent them.
func TestFromModelLeavesWhatItDoesNotKnow(t *testing.T) {
	v := Of(claudeArray())
	for _, tc := range []struct{ name, args string }{
		{"read", `{"file_path":"a"}`},
		{"mcp__blender__render", `{"x":1}`},
		{"Frobnicate", `{}`},
		{"Bash", `{"command": "unterminated`},
		{"Read", `{"file_path":  "spacing kept"}`},
	} {
		name, args := v.FromModel(tc.name, tc.args)
		wantName := tc.name
		if tc.name == "Bash" {
			wantName = "bash"
		}
		if tc.name == "Read" {
			wantName = "read"
		}
		if name != wantName || args != tc.args {
			t.Errorf("FromModel(%q, %q) = (%q, %q), want (%q, the arguments unchanged)", tc.name, tc.args, name, args, wantName)
		}
	}
}

// TestToModelUndoesFromModel pins the history direction: a canonical call
// the log holds renders back as the call the model made.
func TestToModelUndoesFromModel(t *testing.T) {
	v := Of(claudeArray())
	for _, tc := range []struct{ name, args string }{
		{"Bash", `{"command":"ls","timeout":1000}`},
		{"TaskOutput", `{"task_id":"bash_1","block":false,"timeout":5000}`},
		{"TaskStop", `{"task_id":"bash_2"}`},
		{"TaskCreate", `{"subject":"a","description":"b","activeForm":"c"}`},
		{"Grep", `{"pattern":"x","-i":true}`},
	} {
		cname, cargs := v.FromModel(tc.name, tc.args)
		name, args := v.ToModel(cname, cargs)
		if name != tc.name {
			t.Errorf("ToModel(%s) name = %q, want %q", cname, name, tc.name)
			continue
		}
		sameJSON(t, args, tc.args)
	}
}

// TestIdentityVocabulary pins that an array in the harness's own names, and
// the zero value, translate nothing.
func TestIdentityVocabulary(t *testing.T) {
	for _, v := range []Vocabulary{{}, Of(array("read", "bash", "job_output", "job_list"))} {
		if v.ClaudeCode() {
			t.Error("a canonical array reports a Claude Code vocabulary")
		}
		if got := v.Exposed("job_output"); got != "job_output" {
			t.Errorf("Exposed(job_output) = %q", got)
		}
		if got := v.ExposedArg("job_output", "job_id"); got != "job_id" {
			t.Errorf("ExposedArg(job_output, job_id) = %q", got)
		}
		if name, args := v.FromModel("bash", `{"timeoutMs":5}`); name != "bash" || args != `{"timeoutMs":5}` {
			t.Errorf("FromModel(bash) = (%q, %q)", name, args)
		}
		if !v.Offers("job_list") {
			t.Error("a canonical array carrying job_list does not offer it")
		}
	}
	if (Vocabulary{}).Offers("anything") != true {
		t.Error("the zero value must offer every tool")
	}
}

// TestClaudeVocabularyNames pins the names the rest of the harness asks the
// vocabulary for: the prompt, the notice and the background reply.
func TestClaudeVocabularyNames(t *testing.T) {
	v := Of(claudeArray())
	if !v.ClaudeCode() {
		t.Fatal("Claude Code's array does not report its vocabulary")
	}
	for canonical, want := range map[string]string{
		"read": "Read", "job_output": "TaskOutput", "job_kill": "TaskStop", "task": "Agent", "apply_patch": "apply_patch",
	} {
		if got := v.Exposed(canonical); got != want {
			t.Errorf("Exposed(%s) = %q, want %q", canonical, got, want)
		}
	}
	if got := v.ExposedArg("job_output", "job_id"); got != "task_id" {
		t.Errorf("ExposedArg(job_output, job_id) = %q, want task_id", got)
	}
	if v.Offers("job_list") || v.Offers("complete") {
		t.Error("Claude Code's array offers a tool it does not carry")
	}
	if !v.Offers("job_output") {
		t.Error("Claude Code's array does not offer job_output under TaskOutput")
	}
}

// TestItemsToModelCopiesRatherThanMutates pins that the fold's own slice
// is never written to: it is read again after the request is built.
func TestItemsToModelCopiesRatherThanMutates(t *testing.T) {
	v := Of(claudeArray())
	items := []wire.Item{
		wire.UserItem("go"),
		wire.FunctionCallItem("toolu_1", "bash", `{"command":"ls","timeoutMs":10}`),
		wire.FunctionCallItem("toolu_2", "mcp__s__t", `{}`),
	}
	before := append([]wire.Item(nil), items...)
	got := v.ItemsToModel(items)
	if !reflect.DeepEqual(items, before) {
		t.Fatal("ItemsToModel modified its input")
	}
	if got[1].Name != "Bash" {
		t.Errorf("function_call name = %q, want Bash", got[1].Name)
	}
	sameJSON(t, got[1].Arguments, `{"command":"ls","timeout":10}`)
	if !reflect.DeepEqual(got[2], items[2]) {
		t.Error("an MCP call changed")
	}
	if out := (Vocabulary{}).ItemsToModel(items); &out[0] != &items[0] {
		t.Error("the identity vocabulary copied a slice it had no reason to")
	}
}

// TestOfSchemaReadsTheRowsArray pins the fold's path in: the vocabulary is
// recovered from the serialised array the session row froze.
func TestOfSchemaReadsTheRowsArray(t *testing.T) {
	raw, err := json.Marshal(claudeArray())
	if err != nil {
		t.Fatal(err)
	}
	if !OfSchema(raw).ClaudeCode() {
		t.Error("OfSchema lost the Claude Code vocabulary")
	}
	for _, bad := range []string{"", "not json", "{}"} {
		if OfSchema(json.RawMessage(bad)).ClaudeCode() {
			t.Errorf("OfSchema(%q) reports a Claude Code vocabulary", bad)
		}
	}
}

// TestClaudeCodeNamesAreUnique pins the table's own consistency: one entry
// per canonical tool and per Claude Code name.
func TestClaudeCodeNamesAreUnique(t *testing.T) {
	canon, exposed := map[string]bool{}, map[string]bool{}
	for _, e := range claudeCode {
		if canon[e.canonical] || exposed[e.exposed] {
			t.Errorf("duplicate entry %s/%s", e.canonical, e.exposed)
		}
		canon[e.canonical], exposed[e.exposed] = true, true
		if got, ok := ClaudeCodeName(e.canonical); !ok || got != e.exposed {
			t.Errorf("ClaudeCodeName(%s) = %q, %v", e.canonical, got, ok)
		}
	}
}
