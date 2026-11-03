package guard

import (
	"strings"
	"unicode"
)

// The guard rules were written in Python, whose str methods and re module
// classify characters by Unicode. These helpers match them on the characters
// that differ from Go's unicode package.

// spaceClass is the body of a regexp character class equal to Python's \s.
const spaceClass = `\t-\r\x{1c}-\x{20}\x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}`

// wordClass is the body of a regexp character class equal to Python's \w.
const wordClass = `\p{L}\p{N}_`

// pySpace reports whether r is whitespace to Python's str.isspace, str.split,
// str.strip and re's \s.
func pySpace(r rune) bool {
	switch {
	case r >= 0x09 && r <= 0x0d, r >= 0x1c && r <= 0x20:
		return true
	case r == 0x85, r == 0xa0, r == 0x1680, r == 0x2028, r == 0x2029, r == 0x202f, r == 0x205f, r == 0x3000:
		return true
	case r >= 0x2000 && r <= 0x200a:
		return true
	}
	return false
}

// pyWord reports whether r matches Python's re \w.
func pyWord(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r)
}

// pyDigit reports whether r is a digit to Python's str.isdigit: the decimal
// digits plus the superscript, subscript and enclosed digits.
func pyDigit(r rune) bool {
	if unicode.IsDigit(r) {
		return true
	}
	for _, span := range nonDecimalDigits {
		if r >= span[0] && r <= span[1] {
			return true
		}
	}
	return false
}

var nonDecimalDigits = [][2]rune{
	{0xb2, 0xb3}, {0xb9, 0xb9}, {0x1369, 0x1371}, {0x19da, 0x19da}, {0x2070, 0x2070}, {0x2074, 0x2079},
	{0x2080, 0x2089}, {0x2460, 0x2468}, {0x2474, 0x247c}, {0x2488, 0x2490}, {0x24ea, 0x24ea},
	{0x24f5, 0x24fd}, {0x24ff, 0x24ff}, {0x2776, 0x277e}, {0x2780, 0x2788}, {0x278a, 0x2792},
	{0x10a40, 0x10a43}, {0x10e60, 0x10e68}, {0x11052, 0x1105a}, {0x1f100, 0x1f10a},
}

// pyIsDigits is str.isdigit: true for a non-empty string of digits.
func pyIsDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !pyDigit(r) {
			return false
		}
	}
	return true
}

// pyFields is str.split() with no argument.
func pyFields(s string) []string { return strings.FieldsFunc(s, pySpace) }

// pyStrip is str.strip() with no argument.
func pyStrip(s string) string { return strings.TrimFunc(s, pySpace) }

// pyRstrip is str.rstrip() with no argument.
func pyRstrip(s string) string { return strings.TrimRightFunc(s, pySpace) }

// pySplitLines is str.splitlines().
func pySplitLines(s string) []string {
	var lines []string
	runes := []rune(s)
	start := 0
	for i := 0; i < len(runes); i++ {
		switch runes[i] {
		case '\n', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			lines = append(lines, string(runes[start:i]))
			start = i + 1
		case '\r':
			lines = append(lines, string(runes[start:i]))
			if i+1 < len(runes) && runes[i+1] == '\n' {
				i++
			}
			start = i + 1
		}
	}
	if start < len(runes) {
		lines = append(lines, string(runes[start:]))
	}
	return lines
}
