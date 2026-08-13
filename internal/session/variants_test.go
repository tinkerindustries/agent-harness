package session

import (
	"strings"
	"testing"
)

// The base variant must render the shipped prompt byte for byte. Anything
// else silently splits the cached prefix every production session shares
// (docs/CACHE.md).
func TestBaseVariantIsTheShippedPromptExactly(t *testing.T) {
	for _, name := range []string{"", BaseVariant} {
		got, err := RenderSystemPromptVariant(name)
		if err != nil {
			t.Fatalf("RenderSystemPromptVariant(%q): %v", name, err)
		}
		if got != RenderSystemPrompt() {
			t.Errorf("variant %q does not render the shipped prompt exactly", name)
		}
	}
}

// Every variant is a set of replacements against the base prompt. A key that
// the base no longer contains means the prompt was edited out from under the
// variant, and the variant must fail rather than render unchanged and be
// scored as though it had applied.
func TestEveryVariantAppliesToTheCurrentPrompt(t *testing.T) {
	for _, name := range VariantNames() {
		got, err := RenderSystemPromptVariant(name)
		if err != nil {
			t.Errorf("variant %q no longer applies to the base prompt: %v", name, err)
			continue
		}
		if name != BaseVariant && got == RenderSystemPrompt() {
			t.Errorf("variant %q rendered the base prompt unchanged", name)
		}
		if VariantDescription(name) == "" {
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

	if _, err := RenderSystemPromptVariant("test-stale"); err == nil {
		t.Fatal("expected a stale replacement to fail")
	}
}

func TestValidateVariantRejectsAnUnknownName(t *testing.T) {
	if err := ValidateVariant("no-such-variant"); err == nil {
		t.Fatal("expected an unknown variant to be rejected")
	}
	for _, name := range append([]string{""}, VariantNames()...) {
		if err := ValidateVariant(name); err != nil {
			t.Errorf("ValidateVariant(%q) = %v, want nil", name, err)
		}
	}
}

// The search variants exist to move sessions off shell grep, so each must
// still name the tools it is steering toward.
func TestSearchVariantsNameTheSearchTools(t *testing.T) {
	for _, name := range VariantNames() {
		if !strings.HasPrefix(name, "search") {
			continue
		}
		got, err := RenderSystemPromptVariant(name)
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

func TestVariantNamesListsBaseFirst(t *testing.T) {
	names := VariantNames()
	if len(names) == 0 || names[0] != BaseVariant {
		t.Errorf("VariantNames() = %v, want %q first", names, BaseVariant)
	}
}
