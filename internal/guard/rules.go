package guard

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Rule names a Rules checker returns.
const (
	RuleMergePullRequest  = "merge_pull_request"
	RulePublish           = "publish"
	RuleSudo              = "sudo"
	RuleRemoteShell       = "remote_shell"
	RuleScheduler         = "scheduler"
	RulePushDefaultBranch = "push_default_branch"
)

var defaultBranches = map[string]bool{"main": true, "master": true}

const (
	reSpace = `[` + spaceClass + `]`
	reWord  = `[` + wordClass + `]`
	// wordEnd is Python's \b after a word character.
	wordEnd = `(?:$|[^` + wordClass + `])`
)

type rulePattern struct {
	name string
	re   *regexp.Regexp
	// notList marks the crontab rule, whose negative lookahead is checked by hand.
	notList bool
}

// The patterns are the Python RULES with the leading (?:^|(?<=[\s;&|(`])) removed:
// Rules.CheckRules tries each at every position where it holds. They are
// anchored at the start of the text they are given.
var rulePatterns = []rulePattern{
	{name: RuleMergePullRequest, re: compileRule(`gh` + reSpace + `+pr` + reSpace + `+merge` + wordEnd)},
	// \b after "api" is folded into the alternatives so it stays zero-width.
	{name: RuleMergePullRequest, re: compileRule(`gh` + reSpace + `+api(?:/pulls/|[^` + wordClass + `\n][^\n]*/pulls/)\p{Nd}+/merge` + wordEnd)},
	{name: RulePublish, re: compileRule(`(?:npm|pnpm|yarn)` + reSpace + `+(?:publish|release)` + wordEnd)},
	{name: RulePublish, re: compileRule(`npm` + reSpace + `+run` + reSpace + `+release` + wordEnd)},
	{name: RulePublish, re: compileRule(`(?:cargo` + reSpace + `+publish|twine` + reSpace + `+upload|gem` + reSpace + `+push)` + wordEnd)},
	{name: RulePublish, re: compileRule(`gh` + reSpace + `+release` + reSpace + `+(?:create|upload|delete|edit)` + wordEnd)},
	{name: RuleSudo, re: compileRule(`sudo` + reSpace)},
	{name: RuleRemoteShell, re: compileRule(`(?:ssh|scp|sftp)` + reSpace)},
	{name: RuleRemoteShell, re: compileRule(`rsync` + reSpace + `[^\n;&|]*` + reSpace + `[` + wordClass + `.@-]+:`)},
	{name: RuleScheduler, re: compileRule(`crontab` + reSpace + `+`), notList: true},
	{name: RuleScheduler, re: compileRule(`launchctl` + reSpace + `+(?:load|bootstrap|enable|submit)` + wordEnd)},
}

func compileRule(body string) *regexp.Regexp {
	return regexp.MustCompile(`^(?:` + body + `)`)
}

var segmentSplit = regexp.MustCompile("&&|\\|\\||;|\\||\\n|\\$\\(|`|\\(|\\)")

// Rules checks a command against the exact-pattern rules. The zero value is
// ready to use.
type Rules struct{}

// NewRules returns the rule checker.
func NewRules() Rules { return Rules{} }

// CheckRules returns the names of the rules the command breaks, in a stable
// order without repeats. It satisfies RuleChecker.
func (Rules) CheckRules(command, branch string) []string {
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
	for _, p := range rulePatterns {
		if p.matchesAnywhere(text) {
			add(p.name)
		}
	}
	for _, segment := range segmentSplit.Split(text, -1) {
		if pushTargetsDefaultBranch(pyStrip(segment), branch) {
			add(RulePushDefaultBranch)
			break
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
		if !p.notList || crontabArgument(text[i+loc[1]:]) {
			return true
		}
	}
	return false
}

// crontabArgument checks what follows "crontab" and its whitespace: a
// non-space character that does not begin the word -l. This is the rule's
// (?!-l\b)\S.
func crontabArgument(rest string) bool {
	if rest == "" {
		return false
	}
	if strings.HasPrefix(rest, "-l") {
		next, size := utf8.DecodeRuneInString(rest[2:])
		if size == 0 || !pyWord(next) {
			return false
		}
	}
	return true
}

func pushTargetsDefaultBranch(segment, branch string) bool {
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
