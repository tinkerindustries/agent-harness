package tools

import (
	"strings"
	"testing"
)

// TestMCPListResourcesNamesServerKindAndURI pins that a listing tells the
// model the three things it needs to read one: which server holds it,
// whether it is a template it must fill in first, and the URI itself.
func TestMCPListResourcesNamesServerKindAndURI(t *testing.T) {
	e, _ := newTestExecutor(t)
	e.MCP = &fakeMCPProvider{resources: []MCPResource{
		{Server: "docs", URI: "file:///readme.md", Name: "readme", Description: "the readme"},
		{Server: "docs", URI: "file:///page/{slug}", Name: "page", Template: true},
	}}

	res := runTool(t, e, "MCPListResources", struct{}{})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	for _, want := range []string{"docs", "resource", "file:///readme.md", "template", "file:///page/{slug}"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("listing should mention %q, got:\n%s", want, res.Content)
		}
	}
}

// TestMCPListResourcesFiltersByServer covers the one argument it takes.
func TestMCPListResourcesFiltersByServer(t *testing.T) {
	e, _ := newTestExecutor(t)
	e.MCP = &fakeMCPProvider{resources: []MCPResource{
		{Server: "docs", URI: "file:///readme.md"},
		{Server: "other", URI: "file:///elsewhere.md"},
	}}

	res := runTool(t, e, "MCPListResources", map[string]string{"server": "docs"})
	if strings.Contains(res.Content, "elsewhere") {
		t.Errorf("filtered listing should not carry another server's resources, got:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "readme") {
		t.Errorf("filtered listing should carry the named server's own, got:\n%s", res.Content)
	}
}

// TestMCPListResourcesSaysSoWhenThereAreNone pins that an empty answer
// reads as an answer rather than as an empty result the model might retry.
func TestMCPListResourcesSaysSoWhenThereAreNone(t *testing.T) {
	e, _ := newTestExecutor(t)
	e.MCP = &fakeMCPProvider{}

	res := runTool(t, e, "MCPListResources", struct{}{})
	if res.IsError {
		t.Fatalf("no resources is not an error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "No configured MCP server advertises any resources") {
		t.Fatalf("got: %s", res.Content)
	}
}

// TestMCPReadResourceReturnsTheContent covers the live read.
func TestMCPReadResourceReturnsTheContent(t *testing.T) {
	e, _ := newTestExecutor(t)
	e.MCP = &fakeMCPProvider{readContent: map[string]MCPContent{
		"docs file:///readme.md": {Text: "# Readme"},
	}}

	res := runTool(t, e, "MCPReadResource", map[string]string{"server": "docs", "uri": "file:///readme.md"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if res.Content != "# Readme" {
		t.Fatalf("Content = %q", res.Content)
	}
}

// TestMCPReadResourceRequiresBothArguments pins the refusal for a half-made
// call, which is a mistake a model makes far more often than a bad URI.
func TestMCPReadResourceRequiresBothArguments(t *testing.T) {
	e, _ := newTestExecutor(t)
	e.MCP = &fakeMCPProvider{}

	res := runTool(t, e, "MCPReadResource", map[string]string{"uri": "file:///readme.md"})
	if !res.IsError || !strings.Contains(res.Content, "server and uri are required") {
		t.Fatalf("expected a refusal naming both arguments, got: %+v", res)
	}
}

// TestMCPListPromptsNamesArguments pins that a prompt is never listed
// without them: a prompt named alone is a prompt the model can only call
// wrong.
func TestMCPListPromptsNamesArguments(t *testing.T) {
	e, _ := newTestExecutor(t)
	e.MCP = &fakeMCPProvider{prompts: []MCPPrompt{{
		Server: "review", Name: "critique", Description: "critique a diff",
		Arguments: []MCPPromptArg{
			{Name: "diff", Required: true},
			{Name: "tone"},
		},
	}}}

	res := runTool(t, e, "MCPListPrompts", struct{}{})
	for _, want := range []string{"review", "critique", "diff (required)", "tone"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("listing should mention %q, got:\n%s", want, res.Content)
		}
	}
}

// TestMCPGetPromptRendersIt covers the live render.
func TestMCPGetPromptRendersIt(t *testing.T) {
	e, _ := newTestExecutor(t)
	e.MCP = &fakeMCPProvider{readContent: map[string]MCPContent{
		"review critique": {Text: "[user] Critique this"},
	}}

	res := runTool(t, e, "MCPGetPrompt", map[string]any{
		"server": "review", "name": "critique", "arguments": map[string]string{"diff": "x"},
	})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if res.Content != "[user] Critique this" {
		t.Fatalf("Content = %q", res.Content)
	}
}

// TestMCPAccessToolsWithoutAProvider pins that the four say what is wrong
// rather than panicking, for the CLI paths and any context with no MCP
// wired at all.
func TestMCPAccessToolsWithoutAProvider(t *testing.T) {
	e, _ := newTestExecutor(t) // e.MCP left nil
	for _, name := range []string{"MCPListResources", "MCPReadResource", "MCPListPrompts", "MCPGetPrompt"} {
		res := runTool(t, e, name, map[string]string{"server": "s", "uri": "u", "name": "n"})
		if !res.IsError || !strings.Contains(res.Content, "no MCP servers are configured") {
			t.Errorf("%s = %+v, want a plain refusal", name, res)
		}
	}
}

// TestReadOnlyModeGatesTheTwoDiallingAccessTools pins the permission split:
// the two that dial a server answer to the same per-server allowance its
// tools do, while the two that only read this harness's own snapshot are
// always allowed — denying those would refuse a session the ability to find
// out what it is not allowed to read.
func TestReadOnlyModeGatesTheTwoDiallingAccessTools(t *testing.T) {
	p := &Policy{Mode: ModeReadOnly, MCPReadOnlyServers: map[string]bool{"open": true}}

	if d := p.Check("MCPListResources", "MCPListResources shut"); !d.Allow {
		t.Errorf("listing should be allowed in readonly mode: %s", d.Rule)
	}
	if d := p.Check("MCPListPrompts", "MCPListPrompts shut"); !d.Allow {
		t.Errorf("listing prompts should be allowed in readonly mode: %s", d.Rule)
	}
	if d := p.Check("MCPReadResource", "MCPReadResource open"); !d.Allow {
		t.Errorf("reading from a read-only server should be allowed: %s", d.Rule)
	}
	if d := p.Check("MCPReadResource", "MCPReadResource shut"); d.Allow {
		t.Error("reading from a server not marked read-only should be denied in readonly mode")
	}
	if d := p.Check("MCPGetPrompt", "MCPGetPrompt shut"); d.Allow {
		t.Error("a prompt from a server not marked read-only should be denied in readonly mode")
	}
}
