package mcp

import (
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mrgeoffrich/deepseek-harness/internal/agentmeta"
)

// TestNormalizeClientInfoName exercises the mapping from an MCP clientInfo
// name to the agentmeta grammar directly: lowercase, runs of non-alphanumeric
// characters collapsed to one hyphen, edge hyphens trimmed, 32-character cap
// with any trailing hyphen the truncation leaves removed, and "" when nothing
// usable survives.
func TestNormalizeClientInfoName(t *testing.T) {
	long := ""
	for i := 0; i < 40; i++ {
		long += "a"
	}
	dashCut := ""
	for i := 0; i < 31; i++ {
		dashCut += "a"
	}
	dashCut += "-b" // the 32nd character is the hyphen, the 33rd the b

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty input", "", ""},
		{"already a slug", "claude-code", "claude-code"},
		{"title case with space", "Claude Code", "claude-code"},
		{"punctuation run collapses", "Cursor,,, Pro", "cursor-pro"},
		{"all punctuation", "!!! ???", ""},
		{"separators around a name", "— deepseek — harness —", "deepseek-harness"},
		{"leading and trailing separators", "--cursor--", "cursor"},
		{"mixed separators", "Cline 3.14__beta", "cline-3-14-beta"},
		{"over-long input truncates", long, long[:32]},
		{"truncation leaves no trailing hyphen", dashCut, dashCut[:31]},
		{"digits allowed", "windsurf-3", "windsurf-3"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := normalizeClientInfoName(c.in)
			if got != c.want {
				t.Errorf("normalizeClientInfoName(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestNormalizeClientInfoNameValidGrammar checks that every non-empty result
// satisfies the grammar the queue and store validate, so a stamped kind can
// never be rejected downstream by agentmeta itself.
func TestNormalizeClientInfoNameValidGrammar(t *testing.T) {
	inputs := []string{
		"claude-code", "Claude Code", "Cursor", "github copilot", "A...B..C",
		"--", "  ", "äöü", "Windsurf Pro 3.1", "deepseek-harness/2.0",
	}
	for _, in := range inputs {
		got := normalizeClientInfoName(in)
		if got == "" {
			continue
		}
		if err := agentmeta.ValidateParentAgentType(got); err != nil {
			t.Errorf("normalizeClientInfoName(%q) = %q fails the agentmeta grammar: %v", in, got, err)
		}
	}
}

// callToolRequestWithClientInfo builds a *mcpsdk.CallToolRequest carrying the
// given client implementation the way the SDK's own ClientInfo() resolves it
// on the modern protocol: per-request _meta, injected by the client library
// on every call. Constructing it directly keeps this unit test off any
// session machinery; the same path is exercised end to end by
// TestMCPServerLaunchStampsParentAgentTypeFromClientInfo.
func callToolRequestWithClientInfo(info *mcpsdk.Implementation) *mcpsdk.CallToolRequest {
	return &mcpsdk.CallToolRequest{
		Params: &mcpsdk.CallToolParamsRaw{
			Meta: mcpsdk.Meta{
				mcpsdk.MetaKeyClientInfo: info,
			},
		},
	}
}

// TestResolveParentAgentTypeStampsClientInfo checks that a usable clientInfo
// name wins over the caller's own assertion, normalised to the agentmeta
// grammar.
func TestResolveParentAgentTypeStampsClientInfo(t *testing.T) {
	req := callToolRequestWithClientInfo(&mcpsdk.Implementation{Name: "Claude Code", Title: "Claude Code", Version: "2.1.227"})
	if got := resolveParentAgentType(req, "cursor"); got != "claude-code" {
		t.Errorf("clientInfo name should win over the assertion: got %q, want %q", got, "claude-code")
	}
}

// TestResolveParentAgentTypeFallbacks pins the degrade-to-today behaviour: a
// nil request, a clientInfo-less request, and a clientInfo whose name
// normalises to nothing usable all fall back to the caller's own assertion,
// and never panic.
func TestResolveParentAgentTypeFallbacks(t *testing.T) {
	if got := resolveParentAgentType(nil, "cursor"); got != "cursor" {
		t.Errorf("nil request: got %q, want the fallback %q", got, "cursor")
	}
	clientInfoNil := &mcpsdk.CallToolRequest{}
	if got := resolveParentAgentType(clientInfoNil, "cursor"); got != "cursor" {
		t.Errorf("clientInfo-less request: got %q, want the fallback %q", got, "cursor")
	}
	unusable := callToolRequestWithClientInfo(&mcpsdk.Implementation{Name: "!!! ???"})
	if got := resolveParentAgentType(unusable, "cursor"); got != "cursor" {
		t.Errorf("unusable clientInfo name: got %q, want the fallback %q", got, "cursor")
	}
}
