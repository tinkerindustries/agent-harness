package guard

import "strings"

// stripHeredocs replaces each heredoc, from its <<WORD header to the line that
// closes it, with a newline. It reproduces the Python pattern
//
//	<<-?\s*(['"]?)(\w+)\1[^\n]*\n.*?\n\s*\2\s*(?:\n|$)
//
// with re.S, including its backtracking: when no closing line matches the whole
// word of an unquoted delimiter, a shorter prefix of the word is tried.
func stripHeredocs(command string) string {
	if !strings.Contains(command, "<<") {
		return command
	}
	r := []rune(command)
	var out strings.Builder
	copied := 0
	for i := 0; i+1 < len(r); {
		if r[i] == '<' && r[i+1] == '<' {
			if end, ok := matchHeredoc(r, i); ok {
				out.WriteString(string(r[copied:i]))
				out.WriteByte('\n')
				i, copied = end, end
				continue
			}
		}
		i++
	}
	out.WriteString(string(r[copied:]))
	return out.String()
}

func skipSpace(r []rune, i int) int {
	for i < len(r) && pySpace(r[i]) {
		i++
	}
	return i
}

func matchHeredoc(r []rune, start int) (int, bool) {
	n := len(r)
	i := start + 2
	if i < n && r[i] == '-' {
		i++
	}
	i = skipSpace(r, i)
	var quote rune
	if i < n && (r[i] == '\'' || r[i] == '"') {
		quote = r[i]
		i++
	}
	wordStart := i
	for i < n && pyWord(r[i]) {
		i++
	}
	wordEnd := i
	if wordEnd == wordStart {
		return 0, false
	}
	var newline int
	lengths := []int{wordEnd - wordStart}
	if quote != 0 {
		if wordEnd >= n || r[wordEnd] != quote {
			return 0, false
		}
		newline = indexRune(r, wordEnd+1, '\n')
	} else {
		newline = indexRune(r, wordEnd, '\n')
		for l := wordEnd - wordStart - 1; l >= 1; l-- {
			lengths = append(lengths, l)
		}
	}
	if newline < 0 {
		return 0, false
	}
	for _, l := range lengths {
		if end, ok := matchHeredocBody(r, newline+1, r[wordStart:wordStart+l]); ok {
			return end, true
		}
	}
	return 0, false
}

func indexRune(r []rune, from int, c rune) int {
	for i := from; i < len(r); i++ {
		if r[i] == c {
			return i
		}
	}
	return -1
}

// matchHeredocBody finds the smallest newline at or after from that begins a
// closing line: whitespace, the delimiter, then whitespace up to a newline or
// the end of the text. It returns the offset after that match.
func matchHeredocBody(r []rune, from int, delim []rune) (int, bool) {
	n := len(r)
	for j := from; j < n; j++ {
		if r[j] != '\n' {
			continue
		}
		k := skipSpace(r, j+1)
		if k+len(delim) > n || string(r[k:k+len(delim)]) != string(delim) {
			continue
		}
		k += len(delim)
		m := skipSpace(r, k)
		if m == n {
			return n, true
		}
		for x := m - 1; x >= k; x-- {
			if r[x] == '\n' {
				return x + 1, true
			}
		}
	}
	return 0, false
}

// stripQuoted replaces each quoted string with ” unless it follows -c or eval,
// where the quotes are dropped and the text kept. It reproduces the Python
// pattern
//
//	(?<![\w-])(-c|eval)?(\s*)('[^']*'|"(?:[^"\\]|\\.)*")
func stripQuoted(command string) string {
	if !strings.ContainsAny(command, `'"`) {
		return command
	}
	r := []rune(command)
	var out strings.Builder
	for i := 0; i < len(r); {
		if i > 0 && (pyWord(r[i-1]) || r[i-1] == '-') {
			out.WriteRune(r[i])
			i++
			continue
		}
		if end, ok := matchQuoted(r, i, &out); ok {
			i = end
			continue
		}
		out.WriteRune(r[i])
		i++
	}
	return out.String()
}

// matchQuoted tries the quoted pattern at start. On a match it writes the
// replacement to out and returns the offset after the match.
func matchQuoted(r []rune, start int, out *strings.Builder) (int, bool) {
	keyword := ""
	switch {
	case hasRunePrefix(r[start:], "-c"):
		keyword = "-c"
	case hasRunePrefix(r[start:], "eval"):
		keyword = "eval"
	}
	if keyword != "" {
		after := start + len([]rune(keyword))
		spaceEnd := skipSpace(r, after)
		if inner, end, ok := quotedString(r, spaceEnd); ok {
			out.WriteString(keyword)
			out.WriteString(string(r[after:spaceEnd]))
			out.WriteByte(' ')
			out.WriteString(inner)
			out.WriteByte(' ')
			return end, true
		}
	}
	spaceEnd := skipSpace(r, start)
	if _, end, ok := quotedString(r, spaceEnd); ok {
		out.WriteString(string(r[start:spaceEnd]))
		out.WriteString("''")
		return end, true
	}
	return 0, false
}

func hasRunePrefix(r []rune, prefix string) bool {
	p := []rune(prefix)
	if len(r) < len(p) {
		return false
	}
	for i := range p {
		if r[i] != p[i] {
			return false
		}
	}
	return true
}

// quotedString matches '[^']*' or "(?:[^"\\]|\\.)*" at i and returns the text
// between the quotes and the offset after the closing quote.
func quotedString(r []rune, i int) (string, int, bool) {
	n := len(r)
	if i >= n {
		return "", 0, false
	}
	switch r[i] {
	case '\'':
		end := indexRune(r, i+1, '\'')
		if end < 0 {
			return "", 0, false
		}
		return string(r[i+1 : end]), end + 1, true
	case '"':
		for k := i + 1; k < n; {
			switch {
			case r[k] == '"':
				return string(r[i+1 : k]), k + 1, true
			case r[k] == '\\':
				if k+1 >= n || r[k+1] == '\n' {
					return "", 0, false
				}
				k += 2
			default:
				k++
			}
		}
	}
	return "", 0, false
}
