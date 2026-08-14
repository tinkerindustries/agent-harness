// Package promptvariant holds the named alternatives to the shipped system
// prompt and the reminder cadences that go with them, so an eval can compare
// two of them (docs/EVALS.md).
//
// It imports nothing from this repository but the wire vocabulary, which is
// what lets the queue validate a variant name without pulling the agent loop
// in behind it. internal/session owns the prompt text; this package owns the
// edits to it.
package promptvariant

import (
	"fmt"
	"sort"
	"strings"
)

// Prompt variants exist so a wording change can be measured before it ships.
// An eval run publishes the same task under two of them and compares what the
// sessions did (internal/evals). Nothing else sets one: a work request with no
// variant gets Base, whose text is the frozen prompt byte for byte, so the
// shared prefix every production session builds is untouched (docs/CACHE.md).
//
// Two variants in flight at once means two cached prefixes rather than one.
// That costs a second cold prefill per variant and nothing after it, because
// each prefix is an independent unit and every session's first request misses
// regardless.

// Base is the shipped prompt, unedited.
const Base = "base"

// A variant is built by replacing whole lines of the base prompt rather than
// by holding a second copy of it, so an edit to the shipped prompt reaches
// every variant and a stale variant cannot silently drift from it.
type variant struct {
	// description says what the variant is testing, and is printed by
	// `harness eval variants`.
	description string
	// replacements maps an exact substring of the base prompt to its
	// replacement. Every key must be present or rendering fails loudly: a
	// variant that silently no-ops would be scored as if it had applied.
	replacements map[string]string
	// dropTools names tools the variant subtracts from the session's tool
	// array. Subtraction only: a variant can remove a tool that exists, it
	// cannot add one that does not. internal/session resolves the array
	// through tools.DefinitionsForVariant, and the prompt's head is
	// assembled from that array — so the inventory sentence, the count
	// word, and every rule about the dropped tool fall out of the head
	// with the tool itself, and a tool-dropping variant needs no
	// replacements entry to stay consistent with its own tool array.
	dropTools []string
	// reminder names a policy from reminders.go. A variant may change the
	// head, the tail, or both: the head is what the model is told once, the
	// tail is what it is told again as the context grows.
	reminder string
}

// searchToolsRule is the base prompt's whole instruction about searching. It
// is one line today, which is what the sessions measured against it ignored:
// 1802 shell greps against 263 Grep calls and 19 Glob calls.
const searchToolsRule = "- Prefer Grep and Glob to orient before reading whole files."

// planRule is the base prompt's opening of the plan rule, present in both
// provider heads. The kimi-steps variant extends it, so its anchor is a
// complete sentence pair rather than a partial line.
const planRule = "- A task that takes three or more steps gets a plan. Call TaskCreate once, at\n" +
	"  the start, with one entry per step."

var variants = map[string]variant{
	Base: {description: "the shipped prompt, unchanged"},

	"search-first": {
		description: "names the Grep and Glob tools as the way to search, and says what Bash is for instead",
		replacements: map[string]string{
			searchToolsRule: "- Search with the Grep and Glob tools, not with Bash. Grep takes a\n" +
				"  regular expression and an optional glob; Glob takes a path pattern like\n" +
				"  **/*.go. Reach for Bash when you need a shell — running a build, a test,\n" +
				"  a git command — not to find a file or a string.",
		},
	},

	"search-remind": {
		description: "the shipped prompt, with the search rule re-stated as the context grows",
		reminder:    "search-64k",
	},

	"search-first-remind": {
		description: "the search-first wording, re-stated as the context grows",
		replacements: map[string]string{
			searchToolsRule: "- Search with the Grep and Glob tools, not with Bash. Grep takes a\n" +
				"  regular expression and an optional glob; Glob takes a path pattern like\n" +
				"  **/*.go. Reach for Bash when you need a shell — running a build, a test,\n" +
				"  a git command — not to find a file or a string.",
		},
		reminder: "search-64k",
	},

	"search-cost": {
		description: "gives the reason to prefer the search tools rather than only the instruction",
		replacements: map[string]string{
			searchToolsRule: "- Search with the Grep and Glob tools rather than shelling out. They\n" +
				"  return the same answer in one call, already scoped to the workspace, and\n" +
				"  they cannot fail on a flag the shell's own grep does not have.",
		},
	},

	// kimi-steps is the wording arm of the Kimi prompt A/B
	// (docs/KIMI-INTEGRATION.md §4.4, Phase 9). The other arm is base on
	// kimi-k3: a Kimi session's shipped head now carries the corrected
	// fourteen-tool inventory (internal/session), and this variant adds
	// step-by-step execution wording drawn from Kimi's own prompt guidance
	// (third_party/kimi-docs/guide/prompt-best-practice.md, "Clearly Define
	// the Steps Needed to Complete the Task"). Running the same suite under
	// base and kimi-steps on kimi-k3 isolates the wording from the
	// inventory correction, which is not what the eval measures.
	"kimi-steps": {
		description: "the corrected Kimi prompt plus a step-by-step execution rule from Kimi's own prompt guidance; compare with base on kimi-k3 to isolate the wording",
		replacements: map[string]string{
			planRule: planRule + " Then execute the plan step by step: finish one step and check its result before starting the next.",
		},
	},

	// no-bash is the tool-dropping arm: the session is offered no Bash
	// tool, so the shell rule, the batch rule and every other fragment that
	// needs Bash fall out of the head with it, and the inventory and its
	// count follow the array — no replacements entry is needed, and one
	// that restated the inventory would be the drift this mechanism
	// exists to remove. What it measures is whether a session routes
	// around a missing tool instead of stalling on it.
	"no-bash": {
		description: "the shipped prompt with the Bash tool dropped — no shell rule, no batch rule naming Bash, and an inventory that names the sixteen tools the session is actually sent",
		dropTools:   []string{"Bash"},
	},
}

// Apply returns base with the named variant's edits made. An empty name is
// Base, which returns base unchanged.
func Apply(name, base string) (string, error) {
	if name == "" {
		name = Base
	}
	v, ok := variants[name]
	if !ok {
		return "", fmt.Errorf("promptvariant: unknown variant %q; known variants are %s",
			name, strings.Join(Names(), ", "))
	}
	prompt := base
	for from, to := range v.replacements {
		if !strings.Contains(prompt, from) {
			return "", fmt.Errorf("promptvariant: variant %q expects text the base prompt no longer contains: %q", name, from)
		}
		prompt = strings.Replace(prompt, from, to, 1)
	}
	return prompt, nil
}

// Validate reports whether a name is one this build knows, without rendering
// it. Request validation uses this so a typo is rejected at publish rather
// than after a workspace has been cloned.
func Validate(name string) error {
	if name == "" || name == Base {
		return nil
	}
	if _, ok := variants[name]; !ok {
		return fmt.Errorf("unknown prompt variant %q; known variants are %s", name, strings.Join(Names(), ", "))
	}
	return nil
}

// Names lists every known variant, base first and the rest sorted.
func Names() []string {
	rest := make([]string, 0, len(variants))
	for name := range variants {
		if name != Base {
			rest = append(rest, name)
		}
	}
	sort.Strings(rest)
	return append([]string{Base}, rest...)
}

// ReminderPolicyFor is the reminder policy a variant asks for, or the empty
// string when it changes only the prompt.
func ReminderPolicyFor(name string) string {
	if name == "" {
		name = Base
	}
	return variants[name].reminder
}

// ToolsDroppedBy is the set of tools a variant subtracts from the session's
// tool array, or nil for a variant that drops none. Subtraction only: a
// variant can only remove tools that exist — it cannot add one — so the
// caller applies the names with `without` and the array can only shrink.
func ToolsDroppedBy(name string) []string {
	if name == "" {
		name = Base
	}
	return variants[name].dropTools
}

// Description is the one-line summary of what a variant changes.
func Description(name string) string {
	if name == "" {
		name = Base
	}
	return variants[name].description
}
