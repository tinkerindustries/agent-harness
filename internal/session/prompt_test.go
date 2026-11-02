package session

import (
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/tools"
)

// TestPromptNamesExactlyTheToolArray pins each provider's system prompt to
// that provider's tool array: the prompt's "Tools:" inventory must name
// exactly the tools in the array — no more, no less — and the availability
// count ("All seventeen are always available") must match the array length.
//
// The tool array is per-provider, and the system prompt is a second frozen
// head that nothing else ties to it: the base head names twenty-three tools,
// DeepSeek's ten and Kimi's seventeen, and each must be restated the moment
// a tool is added to or dropped from that array. The head is assembled from
// the array (internal/session/prompt.go), so this test pins the assembly:
// the rendered inventory must still name exactly the tools the array
// carries. Without it the two can drift, which is the bug this pins: Kimi
// sessions were once sent an array whose size the prompt did not match.
func TestPromptNamesExactlyTheToolArray(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model string
	}{{"deepseek", "deepseek-v4-pro"}, {"deepseek-flash", "deepseek-flash"}, {"kimi", "kimi-k3"}} {
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
			}{{"deepseek", "deepseek-v4-pro"}, {"deepseek-flash", "deepseek-flash"}, {"kimi", "kimi-k3"}} {
				if other.name == tc.name {
					continue
				}
				for _, tool := range tools.DefinitionsFor(other.model) {
					if containsSorted(want, tool.Function.Name) {
						continue
					}
					// Word boundaries: the names are lower_snake_case now,
					// and a bare substring test would report "job_list"
					// as a mention of "list".
					mention := regexp.MustCompile(`\b` + regexp.QuoteMeta(tool.Function.Name) + `\b`)
					if mention.MatchString(prompt) {
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
// Dropping Bash, BashOutput, and KillBash names three fewer tools in the
// inventory, corrects the count word, and takes every rule gated on Bash —
// the shell rule, the batch rule, the background-shell rule, and the plan
// bullet that mentions "a long run of Bash or Edit calls" — out of the
// text, while every rule not about Bash survives untouched. The plan group
// itself stays: it is gated on the four plan tools, and its one
// Bash-naming bullet picks its wording from what is present, so a no-bash
// head carries the same bullet without the mention.
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

	if len(names) != len(baseNames)-4 {
		t.Fatalf("no-bash head names %d tools, want %d (four fewer than base)", len(names), len(baseNames)-4)
	}
	if containsSorted(names, "bash") {
		t.Error("no-bash inventory still names bash")
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

	// bash's rules are gone with the tool: the shell rule, the batch rule,
	// the background-job rule, and the plan bullet's bash mention are all
	// gated on tools the variant drops, and the inventory must not name bash
	// either — a head that never mentions bash at all is the whole point of
	// dropping the tool.
	for _, gone := range []string{"- The shell is bash on whatever machine you were started on.",
		"- Send independent tool calls together in one message. Several read calls, a",
		"mid-stream through a long run of bash or edit calls"} {
		if strings.Contains(got, gone) {
			t.Errorf("no-bash head still carries the rule %q", gone)
		}
	}
	if strings.Contains(got, "bash") {
		t.Error("no-bash head still mentions bash anywhere")
	}
	// The plan bullet survives without the mention, worded for a session
	// that has no bash.
	for _, keep := range []string{
		"- Set a task to in_progress with task_update before starting it, and to",
		"mid-stream through a long run of edit calls, rather than saving the",
	} {
		if !strings.Contains(got, keep) {
			t.Errorf("no-bash head lost the bash-free plan wording %q", keep)
		}
	}
	// Everything not about bash survives untouched.
	for _, keep := range []string{
		"- Call read on a file before overwriting it with write or changing it with",
		"- Prefer grep and glob to orient before reading whole files.",
		"- A task that takes three or more steps gets a plan. Call task_create once, at",
		"- Tool calls cannot be forced. When the task is done, call complete",
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
// between the two heads: Kimi's inventory names the seventeen tools and not
// the six vision tools, and DeepSeek's names the ten it is offered. The
// search rule is shared, so a change to it lands in both heads; the plan
// rule is Kimi's alone, because DeepSeek's array carries no plan tools.
func TestKimiHeadDiffersFromDeepSeekHeadOnlyWhereItMust(t *testing.T) {
	deepseek, err := RenderSystemPromptFor("deepseek-flash", "")
	if err != nil {
		t.Fatal(err)
	}
	kimi, err := RenderSystemPromptFor("kimi-k3", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"screenshot", "glance", "ground", "detect", "crop", "transcribe"} {
		// Word boundaries: the head says "run_in_background", which is not
		// a mention of the ground tool.
		if regexp.MustCompile(`\b` + gone + `\b`).MatchString(kimi) {
			t.Errorf("Kimi head still mentions %q", gone)
		}
	}
	if !strings.Contains(kimi, "All seventeen are always available") {
		t.Error("Kimi head does not say \"All seventeen are always available\"")
	}
	if !strings.Contains(deepseek, "All ten are always available") {
		t.Error("DeepSeek head does not say \"All ten are always available\"")
	}
	shared := "- Prefer grep and glob to orient before reading whole files."
	if !strings.Contains(kimi, shared) || !strings.Contains(deepseek, shared) {
		t.Errorf("shared rule %q missing from a head", shared)
	}
	plan := "- A task that takes three or more steps gets a plan. Call task_create once, at"
	if !strings.Contains(kimi, plan) {
		t.Errorf("Kimi head lost the plan rule %q", plan)
	}
	if strings.Contains(deepseek, plan) {
		t.Error("DeepSeek head carries the plan rule, but its array has no plan tools")
	}
}
