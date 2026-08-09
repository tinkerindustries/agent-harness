package tools

import (
	"regexp"
	"strings"
)

// globToRegexp compiles a shell-glob-style pattern into a regexp matched
// against a forward-slash relative path. "**" matches any number of path
// segments (including zero), "*" matches within one segment, and "?"
// matches one character within a segment.
func globToRegexp(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); {
		switch {
		case strings.HasPrefix(pattern[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 3
		case strings.HasPrefix(pattern[i:], "**"):
			b.WriteString(".*")
			i += 2
		case pattern[i] == '*':
			b.WriteString("[^/]*")
			i++
		case pattern[i] == '?':
			b.WriteString("[^/]")
			i++
		case strings.ContainsRune(`.+()^$|\{}[]`, rune(pattern[i])):
			b.WriteString(regexp.QuoteMeta(string(pattern[i])))
			i++
		default:
			b.WriteByte(pattern[i])
			i++
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}
