package mcpclient

import (
	"strings"
	"testing"
)

// TestStdioChildEnvFiltersBeforeAppendingServerEnv pins the hosted-mode
// credential boundary at the exact line the design names
// (dial.go:56/docs/STDIO-PROTOCOL.md, "Trust boundaries"): a stdio MCP
// server dialled from `harness gemini-session` must not inherit
// GEMINI_API_KEY, which this process itself was handed only so its own API
// client could reach Google. filter runs against the base environment
// before the server's own configured Env is appended, so a server cannot
// see the stripped variable under its own name either.
func TestStdioChildEnvFiltersBeforeAppendingServerEnv(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "leaked-key")
	t.Setenv("HARNESS_TEST_AMBIENT", "still-here")
	filter := func(in []string) []string {
		out := make([]string, 0, len(in))
		for _, kv := range in {
			if !strings.HasPrefix(kv, "GEMINI_API_KEY=") {
				out = append(out, kv)
			}
		}
		return out
	}

	got := stdioChildEnv(filter, map[string]string{"NODE_ENV": "production"})

	var ambient, nodeEnv bool
	for _, kv := range got {
		if strings.HasPrefix(kv, "GEMINI_API_KEY=") {
			t.Errorf("env = %v, the filtered key survived", got)
		}
		switch kv {
		case "HARNESS_TEST_AMBIENT=still-here":
			ambient = true
		case "NODE_ENV=production":
			nodeEnv = true
		}
	}
	if !ambient {
		t.Errorf("env = %v, want the rest of the base environment still present", got)
	}
	if !nodeEnv {
		t.Errorf("env = %v, want the server's own configured env appended", got)
	}
}

// TestStdioChildEnvWithNoFilterKeepsTheBase pins harness serve's own
// unaffected path: a nil filter is today's behaviour, and this process's own
// environment reaches the child exactly as unfiltered os.Environ() would.
func TestStdioChildEnvWithNoFilterKeepsTheBase(t *testing.T) {
	t.Setenv("HARNESS_TEST_AMBIENT", "still-here")

	got := stdioChildEnv(nil, map[string]string{"X": "y"})

	var ambient, serverEnv bool
	for _, kv := range got {
		switch kv {
		case "HARNESS_TEST_AMBIENT=still-here":
			ambient = true
		case "X=y":
			serverEnv = true
		}
	}
	if !ambient {
		t.Errorf("env = %v, want this process's own environment unfiltered", got)
	}
	if !serverEnv {
		t.Errorf("env = %v, want the server's own configured env appended even with no filter", got)
	}
}
