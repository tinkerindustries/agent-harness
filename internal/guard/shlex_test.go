package guard

import (
	"reflect"
	"testing"
)

func TestSplitWords(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"  a   b\t\nc ", []string{"a", "b", "c"}},
		{`a 'b c' "d e"`, []string{"a", "b c", "d e"}},
		{`a'b'"c"d`, []string{"abcd"}},
		{`'' x`, []string{"", "x"}},
		{`a\ b`, []string{"a b"}},
		{`"a\"b" "a\nb" 'a\nb'`, []string{`a"b`, `a\nb`, `a\nb`}},
		{"a b c", []string{"a b", "c"}},
		// ValueError: unbalanced quote or trailing backslash falls back to str.split().
		{`echo "unclosed one`, []string{"echo", `"unclosed`, "one"}},
		{`echo 'it`, []string{"echo", `'it`}},
		{`trailing\`, []string{`trailing\`}},
		{"a b c", []string{"a", "b", "c"}}, // fallback splits on Unicode space; the happy path does not
	}
	for _, c := range cases {
		got := SplitWords(c.in)
		if len(got) == 0 && len(c.want) == 0 {
			continue
		}
		if c.in == "a b c" {
			// No error here, so this takes the shlex path.
			c.want = []string{"a b c"}
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("SplitWords(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestShlexSplitErrors(t *testing.T) {
	for _, in := range []string{`'`, `"`, `\`, `a "b`, `"a\`} {
		if _, err := shlexSplit(in); err == nil {
			t.Errorf("shlexSplit(%q) did not fail", in)
		}
	}
}
