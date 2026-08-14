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

// TestToolOrderExactlyMatchesTheToolArray pins toolOrder — the order the
// head lists tools in its inventory — to the tool array's membership: the
// same names, each exactly once. TestPromptNamesExactlyTheToolArray cannot
// catch a drift between the two, because it sorts both sides before
// comparing and compares a *rendered* inventory: a name added to toolOrder
// alone is skipped by toolNamesInOrder (it is in no array, so it never
// renders) and a name dropped from the array alone never renders either,
// so the rendered inventory can agree with the array while the two sources
// of truth disagree. The head's order stays toolOrder's — the frozen
// historical order, which is deliberately not the array's own order
// (prompt.go) — but the membership must agree outright, and a tool added
// to one and not the other must fail here the moment it happens.
func TestToolOrderExactlyMatchesTheToolArray(t *testing.T) {
	array := tools.Definitions()
	names := make([]string, 0, len(array))
	for _, tool := range array {
		names = append(names, tool.Function.Name)
	}
	// Same length, and the same names with the same multiplicities — sorted
	// copies, so the comparison is about membership, not order.
	sortedOrder := append([]string(nil), toolOrder...)
	sort.Strings(sortedOrder)
	sortedArray := append([]string(nil), names...)
	sort.Strings(sortedArray)
	if !reflect.DeepEqual(sortedOrder, sortedArray) {
		t.Fatalf("toolOrder names %v, want exactly the tool array's names %v",
			sortedOrder, sortedArray)
	}
	// Each name exactly once on each side: a duplicate in toolOrder (or in
	// the array) would survive the sorted comparison only if mirrored on the
	// other side, which is exactly the kind of drift this test exists to
	// catch.
	for _, side := range []struct {
		what  string
		names []string
	}{{"toolOrder", toolOrder}, {"the tool array", names}} {
		seen := map[string]int{}
		for _, name := range side.names {
			seen[name]++
		}
		for name, n := range seen {
			if n != 1 {
				t.Errorf("%s carries %q %d times, want exactly once", side.what, name, n)
			}
		}
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

// TestVariantDroppedToolRendersAHeadConsistentWithItsArray pins the payoff
// of tool-dropping variants: a variant that subtracts a tool needs no
// replacements entry, because the head is assembled from the session's
// tool array (tools.DefinitionsForVariant) rather than from stored text.
// Dropping Bash names one fewer tool in the inventory, corrects the count
// word, and takes every rule gated on Bash — the shell rule, the batch
// rule, and the plan bullet that mentions "a long run of Bash or Edit
// calls" — out of the text, while every rule not about Bash survives
// untouched. The plan group itself stays: it is gated on the four plan
// tools, and its one Bash-naming bullet picks its wording from what is
// present, so a no-bash head carries the same bullet without the mention.
func TestVariantDroppedToolRendersAHeadConsistentWithItsArray(t *testing.T) {
	base, err := RenderSystemPromptFor("deepseek-v4-pro", "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := RenderSystemPromptFor("deepseek-v4-pro", "no-bash")
	if err != nil {
		t.Fatal(err)
	}

	baseNames, _ := inventoryFromPrompt(t, base)
	names, countWord := inventoryFromPrompt(t, got)

	if len(names) != len(baseNames)-1 {
		t.Fatalf("no-bash head names %d tools, want %d (one fewer than base)", len(names), len(baseNames)-1)
	}
	if containsSorted(names, "Bash") {
		t.Error("no-bash inventory still names Bash")
	}
	// The inventory must agree with the array the variant's session is
	// actually sent — the whole point of generating it from the array.
	array := tools.DefinitionsForVariant("deepseek-v4-pro", "no-bash")
	if len(array) != len(names) {
		t.Fatalf("no-bash head names %d tools but the variant's array has %d", len(names), len(array))
	}
	if word := numberWord(len(array)); countWord != word {
		t.Errorf("no-bash head says %q are always available, want %q for a %d-tool array",
			countWord, word, len(array))
	}

	// Bash's rules are gone with the tool: the shell rule, the batch rule,
	// and the plan bullet's Bash mention are all gated on Bash, and the
	// inventory must not name it either — a head that never mentions Bash at
	// all is the whole point of dropping the tool.
	for _, gone := range []string{"- The shell is bash in an Alpine container",
		"- Send independent tool calls together in one message. Several Reads, a Grep",
		"mid-stream through a long run of Bash or Edit calls"} {
		if strings.Contains(got, gone) {
			t.Errorf("no-bash head still carries the rule %q", gone)
		}
	}
	if strings.Contains(got, "Bash") {
		t.Error("no-bash head still mentions Bash anywhere")
	}
	// The plan bullet survives without the mention, worded for a session
	// that has no Bash.
	for _, keep := range []string{
		"- Set a task to in_progress with TaskUpdate before starting it, and to",
		"mid-stream through a long run of Edit calls, rather than saving the",
	} {
		if !strings.Contains(got, keep) {
			t.Errorf("no-bash head lost the Bash-free plan wording %q", keep)
		}
	}
	// Everything not about Bash survives untouched.
	for _, keep := range []string{
		"- Read a file before Write-ing over it or Edit-ing it. Edit requires an",
		"- Prefer Grep and Glob to orient before reading whole files.",
		"- A task that takes three or more steps gets a plan. Call TaskCreate once, at",
		"- Tool calls cannot be forced. When the task is done, call Complete",
	} {
		if !strings.Contains(got, keep) {
			t.Errorf("no-bash head lost the rule %q", keep)
		}
	}
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
	for _, gone := range []string{"Screenshot", "Glance", "Ground", "Detect", "Crop"} {
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
