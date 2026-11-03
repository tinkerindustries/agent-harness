package guard

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// The prompts and schema ship in the binary from the files classifier-research
// exported, so a prompt change is a file copy and never a Go edit.

//go:embed testdata/assets/stage1/prompt.yaml
var stage1PromptYAML []byte

//go:embed testdata/assets/stage2/AUTONOMY-STAGE2-PROMPT.md
var stage2PromptMarkdown []byte

// stage1Question is the name of the one question in the stage 1 prompt.
const stage1Question = "risk_check"

// stage1Question* keep the key order the hand-off shows: type, instructions,
// criteria.
type stage1QuestionJSON struct {
	Type         string         `json:"type"`
	Instructions string         `json:"instructions"`
	Criteria     stage1Criteria `json:"criteria"`
}

type stage1Criteria struct {
	Allow string `json:"allow"`
	Block string `json:"block"`
}

type stage1PromptFile struct {
	RiskCheck struct {
		Type         string `yaml:"type"`
		Instructions string `yaml:"instructions"`
		Criteria     struct {
			Allow string `yaml:"allow"`
			Block string `yaml:"block"`
		} `yaml:"criteria"`
	} `yaml:"risk_check"`
}

// prompts holds every embedded text the guard sends or versions.
type prompts struct {
	// Stage1Questions is the JSON the evals sent as `questions`.
	Stage1Questions json.RawMessage
	// Stage2System is the fixed system prompt, byte for byte from the fenced
	// block in AUTONOMY-STAGE2-PROMPT.md.
	Stage2System string
	// Stage2Schema is the output_config.format schema, compacted.
	Stage2Schema json.RawMessage
}

func loadPrompts() (prompts, error) {
	q, err := stage1QuestionsJSON(stage1PromptYAML)
	if err != nil {
		return prompts{}, err
	}
	system, err := fencedBlock(stage2PromptMarkdown, "## System prompt", "````text\n", "\n````")
	if err != nil {
		return prompts{}, err
	}
	schemaText, err := fencedBlock(stage2PromptMarkdown, "## Output schema", "```json\n", "\n```")
	if err != nil {
		return prompts{}, err
	}
	var schema bytes.Buffer
	if err := json.Compact(&schema, []byte(schemaText)); err != nil {
		return prompts{}, fmt.Errorf("guard: stage 2 schema: %w", err)
	}
	return prompts{Stage1Questions: q, Stage2System: system, Stage2Schema: schema.Bytes()}, nil
}

func stage1QuestionsJSON(yamlText []byte) (json.RawMessage, error) {
	var file stage1PromptFile
	if err := yaml.Unmarshal(yamlText, &file); err != nil {
		return nil, fmt.Errorf("guard: stage 1 prompt: %w", err)
	}
	q := file.RiskCheck
	if q.Type == "" || q.Instructions == "" || q.Criteria.Allow == "" || q.Criteria.Block == "" {
		return nil, fmt.Errorf("guard: stage 1 prompt: %s is incomplete", stage1Question)
	}
	return marshalNoEscape(map[string]stage1QuestionJSON{
		stage1Question: {Type: q.Type, Instructions: q.Instructions, Criteria: stage1Criteria{Allow: q.Criteria.Allow, Block: q.Criteria.Block}},
	})
}

// fencedBlock returns the text between open and closeMarker, searching after
// the heading.
func fencedBlock(doc []byte, heading, open, closeMarker string) (string, error) {
	text := string(doc)
	at := strings.Index(text, heading)
	if at < 0 {
		return "", fmt.Errorf("guard: stage 2 prompt document has no %q heading", heading)
	}
	text = text[at:]
	start := strings.Index(text, open)
	if start < 0 {
		return "", fmt.Errorf("guard: stage 2 prompt document has no %q block under %q", open, heading)
	}
	text = text[start+len(open):]
	end := strings.Index(text, closeMarker)
	if end < 0 {
		return "", fmt.Errorf("guard: stage 2 prompt document block under %q is not closed", heading)
	}
	return text[:end], nil
}

// marshalNoEscape marshals without HTML escaping, so < > & stay literal as in
// the evals' request bodies.
func marshalNoEscape(v any) ([]byte, error) {
	return marshalIndent(v, "")
}

func marshalIndent(v any, indent string) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if indent != "" {
		enc.SetIndent("", indent)
	}
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
