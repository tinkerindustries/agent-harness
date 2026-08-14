package session

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/tools"
)

// TestPromptNamesExactlyTheToolArray pins each provider's system prompt to
// that provider's tool array: the prompt's "Tools:" inventory must name
// exactly the tools in the array — no more, no less — and the availability
// count ("All fourteen are always available") must match the array length.
//
// This is the guard Phase 8 made necessary (docs/KIMI-INTEGRATION.md §4.4,
// decision 6). The tool array is per-provider, and the system prompt is a
// second frozen head that nothing else ties to it: the DeepSeek head names
// seventeen tools and the Kimi head fourteen, and both must be restated the
// moment a tool is added to or dropped from an array. The head is now
// assembled from the array (internal/session/prompt.go), so this test pins
// the assembly: the rendered inventory must still name exactly the tools
// the array carries. Without it the two can drift — the Phase 9 bug this
// pins is exactly that: Kimi sessions were sent a fourteen-tool array under
// a prompt that named sixteen tools.
func TestPromptNamesExactlyTheToolArray(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model string
	}{{"deepseek", "deepseek-v4-pro"}, {"kimi", "kimi-k3"}} {
		t.Run(tc.name, func(t *testing.T) {
			prompt, err := RenderSystemPromptFor(tc.model, "")
			if err != nil {
				t.Fatalf("RenderSystemPromptFor(%q): %v", tc.model, err)
			}
			array := tools.DefinitionsFor(tc.model)
			got, countWord := inventoryFromPrompt(t, prompt)

			want := make([]string, 0, len(array))
			for _, tool := range array {
				want = append(want, tool.Function.Name)
			}
			sort.Strings(got)
			sort.Strings(want)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s prompt names tools %v, want exactly the %s tool array %v",
					tc.name, got, tc.name, want)
			}
			if word := numberWord(len(array)); countWord != word {
				t.Errorf("%s prompt says %q are always available, want %q for a %d-tool array",
					tc.name, countWord, word, len(array))
			}

			// The inventory is the contract, but a prompt must not name a
			// tool its provider's array does not carry anywhere in the text
			// either: a stray reference to a tool the model was never sent
			// is an instruction it cannot follow.
			for _, other := range []struct {
				name  string
				model string
			}{{"deepseek", "deepseek-v4-pro"}, {"kimi", "kimi-k3"}} {
				if other.name == tc.name {
					continue
				}
				for _, tool := range tools.DefinitionsFor(other.model) {
					if containsSorted(want, tool.Function.Name) {
						continue
					}
					if strings.Contains(prompt, tool.Function.Name) {
						t.Errorf("%s prompt mentions %q, which is not in the %s tool array",
							tc.name, tool.Function.Name, tc.name)
					}
				}
			}
		})
	}
}

// inventoryFromPrompt extracts the names the prompt's "Tools:" paragraph
// lists, in the order written, and the availability count word that follows
// it. The paragraph's shape is part of the frozen prompt: "Tools: A, B, C.\n
// All <n> are always available; ...". A shape change fails here loudly
// rather than silently weakening the assertion above.
func inventoryFromPrompt(t *testing.T, prompt string) ([]string, string) {
	t.Helper()
	_, rest, ok := strings.Cut(prompt, "Tools: ")
	if !ok {
		t.Fatal("prompt has no \"Tools: \" inventory")
	}
	list, rest, ok := strings.Cut(rest, ".\nAll ")
	if !ok {
		t.Fatal("prompt inventory is not followed by \"All <n> are always available\"")
	}
	word, _, ok := strings.Cut(rest, " are always available")
	if !ok {
		t.Fatal("inventory count sentence is missing \" are always available\"")
	}
	parts := strings.Split(list, ",")
	names := make([]string, 0, len(parts))
	for _, p := range parts {
		names = append(names, strings.TrimSpace(p))
	}
	return names, word
}

func containsSorted(sorted []string, name string) bool {
	i := sort.SearchStrings(sorted, name)
	return i < len(sorted) && sorted[i] == name
}

// TestKimiStepsVariantAppliesToTheKimiHead pins the wording variant to the
// head the eval measures it against: the arm is base vs kimi-steps, both on
// kimi-k3, so the variant must render on the Kimi head, differ from it, and
// carry the wording it claims to test.
func TestKimiStepsVariantAppliesToTheKimiHead(t *testing.T) {
	base, err := RenderSystemPromptFor("kimi-k3", "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := RenderSystemPromptFor("kimi-k3", "kimi-steps")
	if err != nil {
		t.Fatal(err)
	}
	if got == base {
		t.Fatal("kimi-steps rendered the Kimi head unchanged")
	}
	for _, want := range []string{"step by step", "check its result"} {
		if !strings.Contains(got, want) {
			t.Errorf("kimi-steps render does not contain %q", want)
		}
	}
}

// TestKimiHeadDiffersFromDeepSeekHeadOnlyWhereItMust pins the relationship
// between the two heads: Kimi's inventory names the fourteen tools and not
// the two vision tools, and the vision rule is the one true sentence. The
// shared rules text is the same in both, so a change to a shared rule lands
// in both heads — that is what the derivation guarantees.
func TestKimiHeadDiffersFromDeepSeekHeadOnlyWhereItMust(t *testing.T) {
	deepseek := RenderSystemPrompt()
	kimi, err := RenderSystemPromptFor("kimi-k3", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"Screenshot", "ReviewScreenshot"} {
		if strings.Contains(kimi, gone) {
			t.Errorf("Kimi head still mentions %q", gone)
		}
	}
	if !strings.Contains(kimi, "All fourteen are always available") {
		t.Error("Kimi head does not say \"All fourteen are always available\"")
	}
	if strings.Contains(deepseek, "All fourteen") {
		t.Error("DeepSeek head mentions fourteen tools; it must stay sixteen")
	}
	// Shared text: the search rule and the plan rule must read identically
	// in both heads.
	for _, shared := range []string{"- Prefer Grep and Glob to orient before reading whole files.",
		"- A task that takes three or more steps gets a plan. Call TaskCreate once, at"} {
		if !strings.Contains(kimi, shared) || !strings.Contains(deepseek, shared) {
			t.Errorf("shared rule %q missing from a head", shared)
		}
	}
}
