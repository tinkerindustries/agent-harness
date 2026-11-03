package guard

import "errors"

var errShlex = errors.New("shlex: no closing quotation or no escaped character")

// SplitWords splits a shell segment into words the way the guard's Python
// source does: shlex.split in POSIX mode without comments, falling back to
// str.split() when shlex raises ValueError.
func SplitWords(segment string) []string {
	words, err := shlexSplit(segment)
	if err != nil {
		return pyFields(segment)
	}
	return words
}

func shlexSpace(r rune) bool { return r == ' ' || r == '\t' || r == '\r' || r == '\n' }

// shlexSplit is shlex.split(s, comments=False): words separated by space, tab,
// CR or LF, with single quotes, double quotes and backslash escapes. It
// returns an error where Python raises ValueError: an unclosed quote or a
// trailing backslash.
func shlexSplit(s string) ([]string, error) {
	rs := []rune(s)
	var out []string
	var token []rune
	state := ' '
	escaped := ' '
	for i := 0; ; i++ {
		has := i < len(rs)
		var c rune
		if has {
			c = rs[i]
		}
		switch state {
		case ' ':
			if !has {
				return out, nil
			}
			switch {
			case shlexSpace(c):
			case c == '\\':
				escaped, state = 'a', '\\'
			case c == '\'' || c == '"':
				state = c
			default:
				token = append(token, c)
				state = 'a'
			}
		case 'a':
			if !has {
				return append(out, string(token)), nil
			}
			switch {
			case shlexSpace(c):
				out = append(out, string(token))
				token, state = nil, ' '
			case c == '\'' || c == '"':
				state = c
			case c == '\\':
				escaped, state = 'a', '\\'
			default:
				token = append(token, c)
			}
		case '\'', '"':
			if !has {
				return nil, errShlex
			}
			switch {
			case c == state:
				state = 'a'
			case c == '\\' && state == '"':
				escaped, state = state, '\\'
			default:
				token = append(token, c)
			}
		case '\\':
			if !has {
				return nil, errShlex
			}
			if (escaped == '\'' || escaped == '"') && c != '\\' && c != escaped {
				token = append(token, '\\')
			}
			token = append(token, c)
			state = escaped
		}
	}
}
