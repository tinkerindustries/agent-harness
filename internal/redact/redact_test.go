package redact

import (
	"strings"
	"testing"
)

func TestStringMasksCredentialShapes(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		token string
	}{
		{"github fine-grained pat", "GITHUB_TOKEN=github_pat_11ABCDEFG0abcdefghijkl_" + strings.Repeat("A", 40),
			"github_pat_11ABCDEFG0abcdefghijkl_" + strings.Repeat("A", 40)},
		{"github classic pat", "token: ghp_" + strings.Repeat("b", 36), "ghp_" + strings.Repeat("b", 36)},
		{"gitlab pat", "glpat-abcdefghij0123456789", "glpat-abcdefghij0123456789"},
		{"deepseek key", "DEEPSEEK_API_KEY=sk-" + strings.Repeat("0", 32), "sk-" + strings.Repeat("0", 32)},
		{"google key", "AIza" + strings.Repeat("x", 35), "AIza" + strings.Repeat("x", 35)},
		{"slack token", "xoxb-1234567890-abcdef", "xoxb-1234567890-abcdef"},
		{"aws key id", "AKIAIOSFODNN7EXAMPLE", "AKIAIOSFODNN7EXAMPLE"},
		{"npm token", "npm_" + strings.Repeat("c", 36), "npm_" + strings.Repeat("c", 36)},
		{"private key", "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\ndef\n-----END OPENSSH PRIVATE KEY-----",
			"-----BEGIN OPENSSH PRIVATE KEY-----\nabc\ndef\n-----END OPENSSH PRIVATE KEY-----"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := String(c.in)
			if strings.Contains(got, c.token) {
				t.Fatalf("token survived redaction: %s", got)
			}
			if !strings.Contains(got, Placeholder) {
				t.Fatalf("expected %s in output, got %s", Placeholder, got)
			}
		})
	}
}

// The transcript is mostly ordinary command output and prose, and redaction
// that ate any of it would be worse than the leak it prevents.
func TestStringLeavesOrdinaryOutputAlone(t *testing.T) {
	for _, s := range []string{
		"ok  	github.com/mrgeoffrich/agent-harness/internal/tools	1.157s",
		"commit 3c5f36bd9e2a1f4c8b7d6e5a4938271605f4e3d2",
		"sk-",
		"the sk-prefix convention",
		"-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----",
		`{"seq":41,"kind":"tool_result","payload":{"content":"total 32\ndrwxr-xr-x"}}`,
	} {
		if got := String(s); got != s {
			t.Errorf("redaction changed ordinary text\n in: %s\nout: %s", s, got)
		}
	}
}

// Redaction runs over marshalled JSON, so a match must never introduce a
// quote or backslash that would break the document it sits inside.
func TestBytesKeepsPayloadValid(t *testing.T) {
	in := []byte(`{"content":"GITHUB_TOKEN=ghp_` + strings.Repeat("z", 36) + `\nDONE"}`)
	out := Bytes(in)
	if strings.Contains(string(out), "ghp_") {
		t.Fatalf("token survived: %s", out)
	}
	if want := `{"content":"GITHUB_TOKEN=` + Placeholder + `\nDONE"}`; string(out) != want {
		t.Fatalf("payload shape changed:\n got: %s\nwant: %s", out, want)
	}
}

func TestBytesReturnsInputWhenNothingMatches(t *testing.T) {
	in := []byte(`{"content":"nothing secret here"}`)
	if got := Bytes(in); string(got) != string(in) {
		t.Fatalf("unexpected rewrite: %s", got)
	}
}
