package guard

import (
	"regexp"
	"strings"
)

// Reasons EscalationReasons returns.
const (
	EscalationSeveralScripts   = "runs_several_scripts"
	EscalationUnscreenedScript = "unscreened_script"
	// EscalationNoStage1 is added by the pipeline, not the detector, when the
	// policy has no stage 1.
	EscalationNoStage1 = "no_stage1"
)

var (
	scriptSplit = regexp.MustCompile("&&|\\|\\||;|\\||\\n|\\$\\(|`|\\(|\\)")
	envAssign   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	interpreter = regexp.MustCompile(`(?:^|/)(?:python(?:\p{Nd}+(?:\.\p{Nd}+)?)?|node|bash|sh|zsh|ruby|perl|deno|bun|tsx|ts-node)$`)
	scriptExt   = regexp.MustCompile(`\.(?:py|mjs|cjs|js|ts|mts|cts|sh|bash|zsh|rb|pl)$`)

	wrappers       = map[string]bool{"time": true, "nohup": true, "exec": true, "env": true, "command": true, "caffeinate": true}
	inlineOptions  = map[string]bool{"-c": true, "-e": true, "--eval": true, "-p": true, "--print": true, "-": true}
	scriptValueOpt = map[string]bool{"--import": true, "--require": true, "-r": true, "--loader": true, "-W": true, "-X": true}
)

// ScriptDetector finds the local scripts a command runs and says whether the
// command should skip stage 1. The zero value is ready to use.
type ScriptDetector struct{}

// NewScriptDetector returns the script detector.
func NewScriptDetector() ScriptDetector { return ScriptDetector{} }

// ExecutedScripts returns the script paths the command runs, as written, in
// order of first appearance and without repeats.
func (ScriptDetector) ExecutedScripts(command string) []string { return ExecutedScripts(command) }

// EscalationReasons says why the command should go straight to stage 2: it
// runs two or more scripts and at least one was written or changed in this
// session, or a session script's content is unknown. It satisfies Escalator.
func (ScriptDetector) EscalationReasons(command string, scripts []ScriptEntry) []string {
	var session []ScriptEntry
	for _, entry := range scripts {
		if entry.Origin == OriginWrittenThisSession || entry.Origin == OriginChangedThisSession {
			session = append(session, entry)
		}
	}
	var reasons []string
	if len(ExecutedScripts(command)) >= 2 && len(session) > 0 {
		reasons = append(reasons, EscalationSeveralScripts)
	}
	for _, entry := range session {
		if entry.Content == nil {
			reasons = append(reasons, EscalationUnscreenedScript)
			break
		}
	}
	return reasons
}

// ExecutedScripts is ScriptDetector.ExecutedScripts.
func ExecutedScripts(command string) []string {
	var found []string
	for _, segment := range scriptSplit.Split(stripHeredocs(command), -1) {
		segment = pyStrip(segment)
		if segment == "" {
			continue
		}
		script := scriptIn(SplitWords(segment))
		if script == "" {
			continue
		}
		seen := false
		for _, f := range found {
			if f == script {
				seen = true
				break
			}
		}
		if !seen {
			found = append(found, script)
		}
	}
	return found
}

// stripPrefix drops leading VAR=value words, wrappers such as time and nohup,
// and the options and count that follow a wrapper.
func stripPrefix(words []string) []string {
	for len(words) > 0 && (envAssign.MatchString(words[0]) || wrappers[words[0]]) {
		words = words[1:]
		if len(words) > 0 && pyIsDigits(strings.TrimLeft(words[0], "-")) {
			words = words[1:]
		}
		for len(words) > 0 && strings.HasPrefix(words[0], "-") && words[0] != "-" {
			words = words[1:]
		}
	}
	return words
}

// firstFile is the first non-option word after an interpreter, or "" for
// inline code and modules.
func firstFile(words []string) string {
	skip := false
	for _, word := range words {
		if skip {
			skip = false
			continue
		}
		if inlineOptions[word] || word == "-m" {
			return ""
		}
		if scriptValueOpt[word] {
			skip = true
			continue
		}
		if strings.HasPrefix(word, "-") {
			continue
		}
		return word
	}
	return ""
}

func withoutOptions(words []string) []string {
	var out []string
	for _, word := range words {
		if !strings.HasPrefix(word, "-") {
			out = append(out, word)
		}
	}
	return out
}

// scriptIn returns the script the words run, or "".
func scriptIn(words []string) string {
	words = stripPrefix(words)
	if len(words) == 0 {
		return ""
	}
	head, rest := words[0], words[1:]
	switch {
	case head == "source" || head == ".":
		if len(rest) > 0 {
			return rest[0]
		}
		return ""
	case head == "uv" && len(rest) > 0 && rest[0] == "run":
		rest = withoutOptions(rest[1:])
		switch {
		case len(rest) > 0 && interpreter.MatchString(rest[0]):
			return scriptIn(rest)
		case len(rest) > 0 && scriptExt.MatchString(rest[0]):
			return rest[0]
		}
		return ""
	case head == "npx" || head == "pnpx" || ((head == "pnpm" || head == "yarn") && len(rest) > 0 && rest[0] == "exec"):
		if head == "pnpm" || head == "yarn" {
			rest = rest[1:]
		}
		rest = withoutOptions(rest)
		if len(rest) == 0 {
			return ""
		}
		return scriptIn(rest)
	case (head == "pnpm" || head == "yarn") && len(rest) > 0 && (rest[0] == "tsx" || rest[0] == "node"):
		return scriptIn(rest)
	case interpreter.MatchString(head):
		target := firstFile(rest)
		if target != "" && (strings.Contains(target, "/") || scriptExt.MatchString(target)) {
			return target
		}
		return ""
	case strings.Contains(head, "/") && scriptExt.MatchString(head):
		return head
	}
	return ""
}
