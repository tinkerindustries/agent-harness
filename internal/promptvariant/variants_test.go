package promptvariant

import (
	"strings"
	"testing"
)

// The base variant returns its input byte for byte. Anything else silently
// splits the cached prefix every production session shares (docs/CACHE.md).
// internal/session asserts the same thing against the real prompt.
func TestBaseVariantIsTheInputExactly(t *testing.T) {
	const base = "a prompt\n" + searchToolsRule + "\nand more"
	for _, name := range []string{"", Base} {
		got, err := Apply(name, base)
		if err != nil {
			t.Fatalf("Apply(%q): %v", name, err)
		}
		if got != base {
			t.Errorf("variant %q does not return its input exactly", name)
		}
	}
}

// testBase carries every anchor a variant replaces, so the table below can be
// applied without the real prompt.
var testBase = "preamble\n" + searchToolsRule + "\npostamble"

// Every variant is a set of replacements against the base prompt. A key that
// the base no longer contains means the prompt was edited out from under the
// variant, and the variant must fail rather than render unchanged and be
// scored as though it had applied.
//
// A variant must differ from the base in something — the prompt, the reminder
// policy, or both. One that differs in neither is an arm that measures
// nothing.
func TestEveryVariantAppliesToTheCurrentPrompt(t *testing.T) {
	for _, name := range Names() {
		got, err := Apply(name, testBase)
		if err != nil {
			t.Errorf("variant %q no longer applies to the base prompt: %v", name, err)
			continue
		}
		if name != Base && got == testBase && ReminderPolicyFor(name) == "" {
			t.Errorf("variant %q changes neither the prompt nor the reminder policy", name)
		}
		if Description(name) == "" {
			t.Errorf("variant %q has no description", name)
		}
	}
}

func TestVariantWithAStaleReplacementFailsLoudly(t *testing.T) {
	variants["test-stale"] = variant{
		description:  "names text the prompt does not contain",
		replacements: map[string]string{"no prompt contains this line": "x"},
	}
	t.Cleanup(func() { delete(variants, "test-stale") })

	if _, err := Apply("test-stale", testBase); err == nil {
		t.Fatal("expected a stale replacement to fail")
	}
}

func TestValidateRejectsAnUnknownName(t *testing.T) {
	if err := Validate("no-such-variant"); err == nil {
		t.Fatal("expected an unknown variant to be rejected")
	}
	for _, name := range append([]string{""}, Names()...) {
		if err := Validate(name); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", name, err)
		}
	}
}

// The search variants exist to move sessions off shell grep, so each must
// still name the tools it is steering toward.
func TestSearchVariantsNameTheSearchTools(t *testing.T) {
	for _, name := range Names() {
		if !strings.HasPrefix(name, "search") || ReminderPolicyFor(name) != "" {
			continue
		}
		got, err := Apply(name, testBase)
		if err != nil {
			t.Fatal(err)
		}
		for _, tool := range []string{"Grep", "Glob"} {
			if !strings.Contains(got, tool) {
				t.Errorf("variant %q does not mention %s", name, tool)
			}
		}
	}
}

func TestNamesListsBaseFirst(t *testing.T) {
	names := Names()
	if len(names) == 0 || names[0] != Base {
		t.Errorf("Names() = %v, want %q first", names, Base)
	}
}
