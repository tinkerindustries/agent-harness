package guard

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/guard/guardtest"
)

func TestLoadBundleValid(t *testing.T) {
	b := mustBundle(t)
	if b.Version() == "" || !strings.HasPrefix(b.Version(), "guard-") {
		t.Fatalf("version = %q", b.Version())
	}
	if b.Cutoff() != guardtest.Cutoff || !b.HasStage1() || b.Stage1Model() != guardtest.Stage1Model || b.Stage2Model() != guardtest.Stage2Model {
		t.Fatalf("accessors: %v %v %q %q", b.Cutoff(), b.HasStage1(), b.Stage1Model(), b.Stage2Model())
	}
	if b.stage1.question != guardtest.Question {
		t.Fatalf("question = %q", b.stage1.question)
	}
	again := mustBundle(t)
	if again.Version() != b.Version() {
		t.Fatal("version is not stable")
	}
}

func TestLoadBundleKeepsQuestionAndSchemaBytes(t *testing.T) {
	spaced := func(b map[string]any) {
		section(b, "stage1")["questions"] = json.RawMessage(`{ "q": { "b": 1, "a": "<&>" } }`)
		section(b, "stage2")["schema"] = json.RawMessage(`{ "z": 1,  "a": [ ] }`)
	}
	b := mustBundle(t, spaced)
	if string(b.stage1.questions) != `{"q":{"b":1,"a":"<&>"}}` {
		t.Fatalf("questions = %s", b.stage1.questions)
	}
	if string(b.stage2.schema) != `{"z":1,"a":[]}` {
		t.Fatalf("schema = %s", b.stage2.schema)
	}
	plain := mustBundle(t, func(m map[string]any) {
		section(m, "stage1")["questions"] = json.RawMessage(`{"q":{"b":1,"a":"<&>"}}`)
		section(m, "stage2")["schema"] = json.RawMessage(`{"z":1,"a":[]}`)
	})
	if plain.Version() != b.Version() {
		t.Fatal("whitespace in the questions changes the version")
	}
}

func TestLoadBundleInvalid(t *testing.T) {
	const marker = "MARKER-TEXT"
	cases := []struct {
		name  string
		edit  func(map[string]any)
		field string
	}{
		{"no schema version", func(b map[string]any) { delete(b, "schema_version") }, "schema_version"},
		{"other schema version", func(b map[string]any) { b["schema_version"] = 2 }, "schema_version"},
		{"cutoff above one", func(b map[string]any) { b["cutoff"] = 1.5 }, "cutoff"},
		{"cutoff below zero", func(b map[string]any) { b["cutoff"] = -0.1 }, "cutoff"},
		{"cutoff missing with stage 1", func(b map[string]any) { delete(b, "cutoff") }, "cutoff"},
		{"cutoff a string", func(b map[string]any) { b["cutoff"] = marker }, "cutoff"},
		{"unknown top-level field", func(b map[string]any) { b[marker] = 1 }, ""},
		{"unknown stage 2 field", func(b map[string]any) { section(b, "stage2")[marker] = 1 }, "stage2"},
		{"unknown pattern field", func(b map[string]any) {
			section(b, "rules")["table"].([]any)[0].(map[string]any)["patterns"].([]any)[0].(map[string]any)[marker] = 1
		}, "rules.table[0].patterns[0]"},
		{"stage 1 model empty", func(b map[string]any) { section(b, "stage1")["model"] = " " }, "stage1.model"},
		{"stage 1 model missing", func(b map[string]any) { delete(section(b, "stage1"), "model") }, "stage1.model"},
		{"stage 2 model empty", func(b map[string]any) { section(b, "stage2")["model"] = "" }, "stage2.model"},
		{"stage 2 missing", func(b map[string]any) { delete(b, "stage2") }, "stage2"},
		{"effort empty", func(b map[string]any) { section(b, "stage2")["effort"] = "" }, "stage2.effort"},
		{"max tokens zero", func(b map[string]any) { section(b, "stage2")["max_tokens"] = 0 }, "stage2.max_tokens"},
		{"max tokens fractional", func(b map[string]any) { section(b, "stage2")["max_tokens"] = 1.5 }, "stage2.max_tokens"},
		{"system empty", func(b map[string]any) { section(b, "stage2")["system"] = "" }, "stage2.system"},
		{"questions not json", func(b map[string]any) { section(b, "stage1")["questions"] = json.RawMessage(`"` + marker + `"`) }, "stage1.questions"},
		{"questions missing", func(b map[string]any) { delete(section(b, "stage1"), "questions") }, "stage1.questions"},
		{"two questions", func(b map[string]any) { section(b, "stage1")["questions"] = json.RawMessage(`{"a":{},"b":{}}`) }, "stage1.questions"},
		{"schema not an object", func(b map[string]any) { section(b, "stage2")["schema"] = json.RawMessage(`[1]`) }, "stage2.schema"},
		{"schema missing", func(b map[string]any) { delete(section(b, "stage2"), "schema") }, "stage2.schema"},
		{"stage 1 fields unknown", func(b map[string]any) {
			section(b, "stage1")["state_fields"] = []any{"proposed_command", marker}
		}, "stage1.state_fields[1]"},
		{"stage 2 fields unknown", func(b map[string]any) {
			section(b, "stage2")["state_fields"] = []any{marker, "proposed_command"}
		}, "stage2.state_fields[0]"},
		{"stage 2 fields without the command", func(b map[string]any) {
			section(b, "stage2")["state_fields"] = []any{"branch"}
		}, "stage2.state_fields"},
		{"stage 1 fields empty", func(b map[string]any) { section(b, "stage1")["state_fields"] = []any{} }, "stage1.state_fields"},
		{"stage 1 fields missing", func(b map[string]any) { delete(section(b, "stage1"), "state_fields") }, "stage1.state_fields"},
		{"fields repeat", func(b map[string]any) {
			section(b, "stage2")["state_fields"] = []any{"proposed_command", "proposed_command"}
		}, "stage2.state_fields[1]"},
		{"rules missing", func(b map[string]any) { delete(b, "rules") }, "rules"},
		{"table missing", func(b map[string]any) { delete(section(b, "rules"), "table") }, "rules.table"},
		{"default branches missing", func(b map[string]any) { delete(section(b, "rules"), "default_branches") }, "rules.default_branches"},
		{"default branch empty", func(b map[string]any) { section(b, "rules")["default_branches"] = []any{"a", ""} }, "rules.default_branches[1]"},
		{"rule without a name", func(b map[string]any) { delete(section(b, "rules")["table"].([]any)[1].(map[string]any), "name") }, "rules.table[1].name"},
		{"rule without patterns", func(b map[string]any) { section(b, "rules")["table"].([]any)[1].(map[string]any)["patterns"] = []any{} }, "rules.table[1].patterns"},
		{"pattern does not compile", func(b map[string]any) { setMatch(b, 2, 1, "(unclosed"+marker) }, "rules.table[2].patterns[1].match"},
		{"pattern uses an unknown macro", func(b map[string]any) { setMatch(b, 0, 0, "x${"+marker+"}") }, "rules.table[0].patterns[0].match"},
		{"pattern matches nothing but empty", func(b map[string]any) { setMatch(b, 1, 0, "(?:x)?") }, "rules.table[1].patterns[0].match"},
		{"pattern missing", func(b map[string]any) {
			delete(section(b, "rules")["table"].([]any)[1].(map[string]any)["patterns"].([]any)[0].(map[string]any), "match")
		}, "rules.table[1].patterns[0].match"},
		{"exception does not compile", func(b map[string]any) {
			section(b, "rules")["table"].([]any)[3].(map[string]any)["patterns"].([]any)[0].(map[string]any)["unless_followed_by"] = "[" + marker
		}, "rules.table[3].patterns[0].unless_followed_by"},
		{"exception uses an unknown macro", func(b map[string]any) {
			section(b, "rules")["table"].([]any)[3].(map[string]any)["patterns"].([]any)[0].(map[string]any)["unless_followed_by"] = "${" + marker + "}"
		}, "rules.table[3].patterns[0].unless_followed_by"},
		{"macro uses an unknown macro", func(b map[string]any) { section(b, "rules", "macros")["space"] = "[${" + marker + "}]" }, "rules.macros"},
		{"macro cycle", func(b map[string]any) {
			m := section(b, "rules", "macros")
			m["a"] = "${b}"
			m["b"] = "${a}"
		}, "rules.macros"},
		{"macro name bad", func(b map[string]any) { section(b, "rules", "macros")["Bad Name"+marker] = "x" }, "rules.macros"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadBundle(guardtest.BundleJSON(tc.edit))
			if err == nil {
				t.Fatal("bundle accepted")
			}
			var be *BundleError
			if !errors.As(err, &be) {
				t.Fatalf("err = %T %v", err, err)
			}
			if be.Field != tc.field {
				t.Fatalf("field = %q, want %q (%v)", be.Field, tc.field, err)
			}
			if strings.Contains(err.Error(), marker) {
				t.Fatalf("error repeats bundle text: %v", err)
			}
			if tc.field != "" && !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("error does not name the field: %v", err)
			}
		})
	}
}

func setMatch(b map[string]any, rule, pattern int, text string) {
	section(b, "rules")["table"].([]any)[rule].(map[string]any)["patterns"].([]any)[pattern].(map[string]any)["match"] = text
}

func TestLoadBundleRefusesNonJSON(t *testing.T) {
	for _, data := range []string{``, `not json`, `[]`, `"x"`, `{"schema_version":1,`} {
		_, err := LoadBundle([]byte(data))
		var be *BundleError
		if !errors.As(err, &be) || be.Field != "" {
			t.Errorf("%q: err = %v", data, err)
		}
	}
}

func TestBundleWithoutStage1(t *testing.T) {
	b := mustBundle(t, func(m map[string]any) {
		delete(m, "stage1")
		delete(m, "cutoff")
	})
	if b.HasStage1() || b.Stage1Model() != "" {
		t.Fatal("stage 1 present")
	}
	if _, err := NewStage1Client(Stage1Config{Endpoint: "http://x", Bundle: b}); err == nil {
		t.Fatal("stage 1 client built without a stage 1")
	}
}

func TestVersionChangesWithEachField(t *testing.T) {
	ref := mustBundle(t).Version()
	rule := func(b map[string]any, i, j int) map[string]any {
		return section(b, "rules")["table"].([]any)[i].(map[string]any)["patterns"].([]any)[j].(map[string]any)
	}
	edits := map[string]func(map[string]any){
		"cutoff":         func(b map[string]any) { b["cutoff"] = 0.31 },
		"stage 1 model":  func(b map[string]any) { section(b, "stage1")["model"] = "other" },
		"stage 1 fields": func(b map[string]any) { section(b, "stage1")["state_fields"] = []any{"proposed_command", "branch"} },
		"stage 1 questions": func(b map[string]any) {
			section(b, "stage1")["questions"] = json.RawMessage(`{"gate_check":{"type":"choice"}}`)
		},
		"no stage 1":    func(b map[string]any) { delete(b, "stage1") },
		"stage 2 model": func(b map[string]any) { section(b, "stage2")["model"] = "other" },
		"effort":        func(b map[string]any) { section(b, "stage2")["effort"] = "high" },
		"max tokens":    func(b map[string]any) { section(b, "stage2")["max_tokens"] = 1235 },
		"stage 2 fields": func(b map[string]any) {
			section(b, "stage2")["state_fields"] = []any{"proposed_command", "branch"}
		},
		"stage 2 field order": func(b map[string]any) {
			section(b, "stage2")["state_fields"] = []any{"proposed_command", "working_directory", "branch", "recent_commands", "scripts_run", "package_scripts"}
		},
		"system": func(b map[string]any) {
			section(b, "stage2")["system"] = "Invented system prompt with <angle> & ampersand!"
		},
		"schema": func(b map[string]any) { section(b, "stage2")["schema"] = json.RawMessage(`{"type":"object"}`) },
		"macro body": func(b map[string]any) {
			section(b, "rules", "macros")["word_chars"] = `\p{L}\p{N}_\$`
		},
		"extra macro":     func(b map[string]any) { section(b, "rules", "macros")["extra"] = "x" },
		"default branch":  func(b map[string]any) { section(b, "rules")["default_branches"] = []any{"trunk"} },
		"no branches":     func(b map[string]any) { section(b, "rules")["default_branches"] = []any{} },
		"rule name":       func(b map[string]any) { section(b, "rules")["table"].([]any)[0].(map[string]any)["name"] = "renamed" },
		"pattern":         func(b map[string]any) { setMatch(b, 0, 1, `ship${space}+them${word_end}`) },
		"exception":       func(b map[string]any) { rule(b, 3, 0)["unless_followed_by"] = `$|-z${word_end}` },
		"exception added": func(b map[string]any) { rule(b, 1, 0)["unless_followed_by"] = `$` },
		"rule order": func(b map[string]any) {
			tbl := section(b, "rules")["table"].([]any)
			tbl[0], tbl[1] = tbl[1], tbl[0]
		},
		"pattern dropped": func(b map[string]any) {
			r := section(b, "rules")["table"].([]any)[0].(map[string]any)
			r["patterns"] = r["patterns"].([]any)[:1]
		},
	}
	seen := map[string]string{ref: "base"}
	for name, edit := range edits {
		v := mustBundle(t, edit).Version()
		if prev, dup := seen[v]; dup {
			t.Errorf("changing %s gives the same version as %s", name, prev)
		}
		seen[v] = name
	}
}

func TestVersionFieldBoundariesDoNotCollide(t *testing.T) {
	a := mustBundle(t, func(b map[string]any) {
		section(b, "stage1")["model"] = "ab"
		section(b, "stage2")["model"] = "c"
	})
	b := mustBundle(t, func(b map[string]any) {
		section(b, "stage1")["model"] = "a"
		section(b, "stage2")["model"] = "bc"
	})
	if a.Version() == b.Version() {
		t.Fatal("shifting a byte between fields keeps the version")
	}
}

func TestStateFieldsNeeded(t *testing.T) {
	full := mustBundle(t).StateFields()
	for _, f := range stateFieldNames {
		if !full[f] {
			t.Errorf("full bundle does not need %s", f)
		}
	}
	reduced := mustBundle(t, func(b map[string]any) {
		section(b, "stage1")["state_fields"] = []any{"proposed_command"}
		section(b, "stage2")["state_fields"] = []any{"proposed_command", "recent_commands"}
		section(b, "rules")["default_branches"] = []any{}
	}).StateFields()
	want := map[string]bool{FieldProposedCommand: true, FieldRecentCommands: true, FieldScriptsRun: true}
	if len(reduced) != len(want) {
		t.Fatalf("needed = %v", reduced)
	}
	for f := range want {
		if !reduced[f] {
			t.Errorf("reduced bundle does not need %s", f)
		}
	}
	withBranch := mustBundle(t, func(b map[string]any) {
		section(b, "stage1")["state_fields"] = []any{"proposed_command"}
		section(b, "stage2")["state_fields"] = []any{"proposed_command"}
	}).StateFields()
	if !withBranch[FieldBranch] || withBranch[FieldPackageScripts] {
		t.Fatalf("needed = %v", withBranch)
	}
}
