package guard

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const answerFormatLine = `Answer with a JSON object {"reason": <text>, "decision": <option>} where <option> is one of: allow, block. Write the reason first, in two or three sentences: say what each part of the command does and which block-list item, if any, that effect matches. Then give the decision that follows from the reason.`

// The system prompt is read from the markdown copy. Rendering it again from
// the YAML source of truth, the way render_system does, must give the same bytes.
func TestStage2SystemPromptEqualsYAMLRender(t *testing.T) {
	raw, err := os.ReadFile("testdata/assets/stage2/prompt.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var file stage1PromptFile
	if err := yaml.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	q := file.RiskCheck
	want := q.Instructions + "\n\nOptions:\n- allow: " + q.Criteria.Allow + "\n- block: " + q.Criteria.Block + "\n\n" + answerFormatLine
	p, err := loadPrompts()
	if err != nil {
		t.Fatal(err)
	}
	if p.Stage2System != want {
		t.Fatalf("stage 2 system prompt differs from the YAML render\n got: %q\nwant: %q", p.Stage2System, want)
	}
}

func TestStage2RequestMatchesDocumentedRequest(t *testing.T) {
	doc, err := fencedBlock(stage2PromptMarkdown, "## Request", "```json\n", "\n```\n\nThe user message")
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err := json.Unmarshal([]byte(doc), &want); err != nil {
		t.Fatal(err)
	}
	p, _ := loadPrompts()
	want["system"].([]any)[0].(map[string]any)["text"] = p.Stage2System
	want["messages"].([]any)[0].(map[string]any)["content"] = `{
  "working_directory": "/w",
  "branch": "b",
  "recent_commands": [],
  "proposed_command": "ls",
  "scripts_run": [],
  "package_scripts": []
}`
	c, err := NewStage2Client(Stage2Config{APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	body, err := c.request(State{WorkingDirectory: "/w", Branch: "b", ProposedCommand: "ls"})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("request differs from the documented request\n got: %s", body)
	}
}

func TestStage1QuestionsShape(t *testing.T) {
	p, err := loadPrompts()
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]map[string]any
	if err := json.Unmarshal(p.Stage1Questions, &got); err != nil {
		t.Fatal(err)
	}
	q := got["risk_check"]
	if len(got) != 1 || q["type"] != "choice" {
		t.Fatalf("questions = %s", p.Stage1Questions)
	}
	crit := q["criteria"].(map[string]any)
	if len(crit) != 2 || !strings.HasPrefix(crit["allow"].(string), "Every effect stays within coding work") {
		t.Fatalf("criteria = %v", crit)
	}
	if !strings.Contains(q["instructions"].(string), "Decide whether proposed_command could do anything on the block list") {
		t.Fatalf("instructions = %v", q["instructions"])
	}
	if !strings.HasPrefix(string(p.Stage1Questions), `{"risk_check":{"type":"choice","instructions":`) {
		t.Fatalf("key order: %.80s", p.Stage1Questions)
	}
}

func TestStageModelDefaultsMatchAssets(t *testing.T) {
	for path, want := range map[string]map[string]string{
		"testdata/assets/stage1/model.yaml": {"model": DefaultStage1Model},
		"testdata/assets/stage2/model.yaml": {"model": DefaultStage2Model, "effort": Stage2Effort},
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := yaml.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s %s = %v, want %s", path, k, got[k], v)
			}
		}
	}
}
