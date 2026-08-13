package session

import (
	"fmt"
	"sort"
	"strings"
)

// Prompt variants exist so a wording change can be measured before it ships.
// An eval run publishes the same task under two of them and compares what the
// sessions did (internal/evals). Nothing else sets one: a work request with no
// variant gets BaseVariant, whose text is the frozen prompt byte for byte, so
// the shared prefix every production session builds is untouched
// (docs/CACHE.md).
//
// Two variants in flight at once means two cached prefixes rather than one.
// That costs a second cold prefill per variant and nothing after it, because
// each prefix is an independent unit and every session's first request misses
// regardless.

// BaseVariant is the shipped prompt. Its rendered text is identical to
// RenderSystemPrompt's.
const BaseVariant = "base"

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
}

// searchToolsRule is the base prompt's whole instruction about searching. It
// is one line today, which is what the sessions measured against it ignored:
// 1802 shell greps against 263 Grep calls and 19 Glob calls.
const searchToolsRule = "- Prefer Grep and Glob to orient before reading whole files."

var variants = map[string]variant{
	BaseVariant: {description: "the shipped prompt, unchanged"},

	"search-first": {
		description: "names the Grep and Glob tools as the way to search, and says what Bash is for instead",
		replacements: map[string]string{
			searchToolsRule: "- Search with the Grep and Glob tools, not with Bash. Grep takes a\n" +
				"  regular expression and an optional glob; Glob takes a path pattern like\n" +
				"  **/*.go. Reach for Bash when you need a shell — running a build, a test,\n" +
				"  a git command — not to find a file or a string.",
		},
	},

	"search-cost": {
		description: "gives the reason to prefer the search tools rather than only the instruction",
		replacements: map[string]string{
			searchToolsRule: "- Search with the Grep and Glob tools rather than shelling out. They\n" +
				"  return the same answer in one call, already scoped to the workspace, and\n" +
				"  they cannot fail on a flag the shell's own grep does not have.",
		},
	},
}

// RenderSystemPromptVariant returns the system prompt for a named variant. An
// empty name is BaseVariant.
func RenderSystemPromptVariant(name string) (string, error) {
	if name == "" {
		name = BaseVariant
	}
	v, ok := variants[name]
	if !ok {
		return "", fmt.Errorf("session: unknown prompt variant %q; known variants are %s",
			name, strings.Join(VariantNames(), ", "))
	}
	prompt := systemPrompt
	for from, to := range v.replacements {
		if !strings.Contains(prompt, from) {
			return "", fmt.Errorf("session: prompt variant %q expects text the base prompt no longer contains: %q", name, from)
		}
		prompt = strings.Replace(prompt, from, to, 1)
	}
	return prompt, nil
}

// ValidateVariant reports whether a name is one this build knows, without
// rendering it. Request validation uses this so a typo is rejected at publish
// rather than after a workspace has been cloned.
func ValidateVariant(name string) error {
	if name == "" || name == BaseVariant {
		return nil
	}
	if _, ok := variants[name]; !ok {
		return fmt.Errorf("unknown prompt variant %q; known variants are %s", name, strings.Join(VariantNames(), ", "))
	}
	return nil
}

// VariantNames lists every known variant, base first and the rest sorted.
func VariantNames() []string {
	rest := make([]string, 0, len(variants))
	for name := range variants {
		if name != BaseVariant {
			rest = append(rest, name)
		}
	}
	sort.Strings(rest)
	return append([]string{BaseVariant}, rest...)
}

// VariantDescription is the one-line summary of what a variant changes.
func VariantDescription(name string) string {
	if name == "" {
		name = BaseVariant
	}
	return variants[name].description
}
