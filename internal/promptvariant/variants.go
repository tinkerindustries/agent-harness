// Package promptvariant holds the named alternatives to the shipped system
// prompt and the reminder cadences that go with them, so an eval can compare
// two of them.
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
// sessions did. Nothing else sets one: a request with no
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
	// description says what the variant is testing, and is what the
	// browser's eval screens print for it.
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

// searchToolsRule is the base prompt's whole instruction about searching and
// reading. The search-* variants replace it with their own wording.
const searchToolsRule = "- Use read to inspect files, with offset and limit for large files. Use glob\n" +
	"  to discover file paths and grep to search file contents. Read matched files\n" +
	"  when you need surrounding context."

// planRule is the base prompt's opening of the plan rule, present in both
// provider heads. The kimi-steps variant extends it, so its anchor is a
// complete sentence pair rather than a partial line.
const planRule = "- A task that takes three or more steps gets a plan. Call task_create once, at\n" +
	"  the start, with one entry per step."

var variants = map[string]variant{
	Base: {description: "the shipped prompt, unchanged"},

	"search-first": {
		description: "names the grep and glob tools as the way to search, and says what bash is for instead",
		replacements: map[string]string{
			searchToolsRule: "- Search with the grep and glob tools, not with bash. grep takes a\n" +
				"  regular expression and an optional glob; glob takes a path pattern like\n" +
				"  **/*.go. Reach for bash when you need a shell — running a build, a test,\n" +
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
			searchToolsRule: "- Search with the grep and glob tools, not with bash. grep takes a\n" +
				"  regular expression and an optional glob; glob takes a path pattern like\n" +
				"  **/*.go. Reach for bash when you need a shell — running a build, a test,\n" +
				"  a git command — not to find a file or a string.",
		},
		reminder: "search-64k",
	},

	"search-cost": {
		description: "gives the reason to prefer the search tools rather than only the instruction",
		replacements: map[string]string{
			searchToolsRule: "- Search with the grep and glob tools rather than shelling out. They\n" +
				"  return the same answer in one call, already scoped to the workspace, and\n" +
				"  they cannot fail on a flag the shell's own grep does not have.",
		},
	},

	// kimi-steps is the wording arm of the Kimi prompt A/B
	// (docs/KIMI-INTEGRATION.md §4.4, Phase 9). The other arm is base on
	// kimi-k3: a Kimi session's shipped head now carries the corrected
	// inventory for its array (internal/session, internal/tools/definitions.go),
	// and this variant adds
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

	// no-bash is the tool-dropping arm: the session is offered no bash
	// tool, so the shell rule, the batch rule and every other fragment that
	// needs bash fall out of the head with it, and the inventory and its
	// count follow the array — no replacements entry is needed, and one
	// that restated the inventory would be the drift this mechanism
	// exists to remove. What it measures is whether a session routes
	// around a missing tool instead of stalling on it. job_output,
	// job_kill and job_list drop with it: none means anything without bash
	// to start a background job for them to act on, and leaving any in the
	// array would put "bash" back in the inventory sentence as a substring
	// of their own names.
	"no-bash": {
		description: "the shipped prompt with bash, job_output, job_kill and job_list dropped — no shell rule, no batch rule naming bash, and an inventory that names only the tools the session is actually sent",
		dropTools:   []string{"bash", "job_output", "job_kill", "job_list"},
	},

	// no-patch is the editing-tool arm: the session is offered write and
	// edit but not apply_patch, so the head's apply_patch rule falls out with
	// the tool and the inventory names only what the session is sent. What
	// it measures is whether a session does better with one editing tool
	// than with two. The plan's risk is that a model picks a patch where edit
	// was more reliable.
	"no-patch": {
		description: "the shipped prompt with apply_patch dropped: the editing rule names write and edit only, and an inventory that names only the tools the session is actually sent; compare with base on the same model",
		dropTools:   []string{"apply_patch"},
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
