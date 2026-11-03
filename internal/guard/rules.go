package guard

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// RulePushDefaultBranch is the name the push check reports. The mechanism
// owns it, so it is not in a bundle's table.
const RulePushDefaultBranch = "push_default_branch"

type rulePattern struct {
	// re is anchored at the start of the text it is matched against.
	re *regexp.Regexp
	// unless, when set, is tested by hand against the text after the match;
	// a match there cancels the hit. It stands in for a negative lookahead.
	unless *regexp.Regexp
}

type ruleEntry struct {
	name     string
	patterns []rulePattern
}

var segmentSplit = regexp.MustCompile("&&|\\|\\||;|\\||\\n|\\$\\(|`|\\(|\\)")

// Rules checks a command against a bundle's rule table. The zero value has no
// rules and no default branches, so it hits nothing. Bundle.Rules builds one.
type Rules struct {
	table           []ruleEntry
	defaultBranches map[string]bool
}

// CheckRules returns the names of the rules the command breaks, in a stable
// order without repeats. It satisfies RuleChecker.
func (r Rules) CheckRules(command, branch string) []string {
	text := stripQuoted(stripHeredocs(command))
	var hits []string
	add := func(name string) {
		for _, h := range hits {
			if h == name {
				return
			}
		}
		hits = append(hits, name)
	}
	for _, entry := range r.table {
		for _, p := range entry.patterns {
			if p.matchesAnywhere(text) {
				add(entry.name)
				break
			}
		}
	}
	if len(r.defaultBranches) > 0 {
		for _, segment := range segmentSplit.Split(text, -1) {
			if pushTargetsDefaultBranch(pyStrip(segment), branch, r.defaultBranches) {
				add(RulePushDefaultBranch)
				break
			}
		}
	}
	return hits
}

// startsCommand reports whether a rule may begin at byte offset i: at the
// start of the text or after whitespace, ; & | ( or a backtick.
func startsCommand(text string, i int) bool {
	if i == 0 {
		return true
	}
	prev, _ := utf8.DecodeLastRuneInString(text[:i])
	switch prev {
	case ';', '&', '|', '(', '`':
		return true
	}
	return pySpace(prev)
}

func (p rulePattern) matchesAnywhere(text string) bool {
	for i := 0; i <= len(text); i++ {
		if i < len(text) && !utf8.RuneStart(text[i]) {
			continue
		}
		if !startsCommand(text, i) {
			continue
		}
		loc := p.re.FindStringIndex(text[i:])
		if loc == nil {
			continue
		}
		if p.unless == nil || !p.unless.MatchString(text[i+loc[1]:]) {
			return true
		}
	}
	return false
}

func pushTargetsDefaultBranch(segment, branch string, defaultBranches map[string]bool) bool {
	words := pyFields(segment)
	if len(words) < 2 || words[0] != "git" {
		return false
	}
	index := -1
	for i, word := range words {
		if word == "push" {
			index = i
			break
		}
	}
	if index < 0 {
		return false
	}
	var args []string
	for _, word := range words[index+1:] {
		if !strings.HasPrefix(word, "-") && !strings.ContainsAny(word, "<>") {
			args = append(args, word)
		}
	}
	for _, word := range words {
		if word == "--all" || word == "--mirror" {
			return true
		}
	}
	var refspecs []string
	if len(args) > 1 {
		refspecs = args[1:]
	}
	for _, refspec := range refspecs {
		parts := strings.Split(strings.TrimLeft(refspec, "+"), ":")
		destination := strings.TrimPrefix(parts[len(parts)-1], "refs/heads/")
		if defaultBranches[destination] {
			return true
		}
	}
	return len(refspecs) == 0 && defaultBranches[branch]
}
