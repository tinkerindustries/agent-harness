package evals

import (
	"os"
	"path/filepath"
	"testing"
)

func writeSuite(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "suite.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const minimalSuite = `{
  "name": "s",
  "rubric": "r",
  "tasks": [{"id": "t", "prompt": "p", "repos": [{"url": "https://example.com/r"}]}]
}`

func TestLoadSuiteReadsATask(t *testing.T) {
	s, err := LoadSuite(writeSuite(t, minimalSuite))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Tasks) != 1 || s.Tasks[0].ID != "t" {
		t.Fatalf("tasks = %+v", s.Tasks)
	}
	if got := s.Tasks[0].PermissionModeOrDefault(); got != "readonly" {
		t.Errorf("default permission mode = %q, want readonly", got)
	}
}

// A misspelled field would otherwise be dropped in silence, and the suite
// would run as something other than what was written.
func TestLoadSuiteRejectsAnUnknownField(t *testing.T) {
	if _, err := LoadSuite(writeSuite(t, `{"name":"s","taks":[]}`)); err == nil {
		t.Fatal("expected an unknown field to be rejected")
	}
}

func TestLoadSuiteRejectsIncompleteTasks(t *testing.T) {
	for name, body := range map[string]string{
		"no name":      `{"tasks":[{"id":"t","prompt":"p","repos":[{"url":"u"}]}]}`,
		"no tasks":     `{"name":"s","tasks":[]}`,
		"no id":        `{"name":"s","tasks":[{"prompt":"p","repos":[{"url":"u"}]}]}`,
		"no prompt":    `{"name":"s","tasks":[{"id":"t","repos":[{"url":"u"}]}]}`,
		"no repos":     `{"name":"s","tasks":[{"id":"t","prompt":"p"}]}`,
		"duplicate id": `{"name":"s","tasks":[{"id":"t","prompt":"p","repos":[{"url":"u"}]},{"id":"t","prompt":"q","repos":[{"url":"u"}]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadSuite(writeSuite(t, body)); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

// Every request an eval publishes must pass the same validation as any other
// producer's, or the suite fails halfway through instead of at the start.
func TestTaskRequestValidates(t *testing.T) {
	s, err := LoadSuite(writeSuite(t, minimalSuite))
	if err != nil {
		t.Fatal(err)
	}
	req := s.Tasks[0].Request("eval-1", "base")
	if err := req.Validate(); err != nil {
		t.Fatalf("request does not validate: %v", err)
	}
	if req.PromptVariant != "base" {
		t.Errorf("prompt_variant = %q, want base", req.PromptVariant)
	}
}

func TestValidateVariantsNeedsTwoDistinctKnownNames(t *testing.T) {
	for name, variants := range map[string][]string{
		"one":       {"base"},
		"duplicate": {"base", "base"},
		"unknown":   {"base", "no-such-variant"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateVariants(variants); err == nil {
				t.Error("expected an error")
			}
		})
	}
	if err := ValidateVariants([]string{"base", "search-first"}); err != nil {
		t.Errorf("ValidateVariants = %v, want nil", err)
	}
}

// Every embedded suite must parse, or the first thing anyone runs fails and
// the HTTP surface cannot list what this build can do.
func TestEmbeddedSuitesLoad(t *testing.T) {
	suites := EmbeddedSuites()
	if len(suites) == 0 {
		t.Fatal("no suites embedded")
	}
	for _, suite := range suites {
		if err := suite.validate(); err != nil {
			t.Errorf("%s: %v", suite.Name, err)
		}
	}
	if _, err := EmbeddedSuite("search"); err != nil {
		t.Errorf("the search suite is not embedded: %v", err)
	}
	if _, err := EmbeddedSuite("no-such-suite"); err == nil {
		t.Error("expected an unknown suite name to fail")
	}
}
