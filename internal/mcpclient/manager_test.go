package mcpclient

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "harness.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustCreateServer(t *testing.T, s *store.Store, srv store.MCPServer) {
	t.Helper()
	if err := s.CreateMCPServer(context.Background(), srv); err != nil {
		t.Fatalf("create mcp server %q: %v", srv.Name, err)
	}
}

func mustSaveProbe(t *testing.T, s *store.Store, name string, tools []store.MCPToolSnapshot) {
	t.Helper()
	if err := s.SaveMCPProbe(context.Background(), name, tools, "", time.Now().UTC()); err != nil {
		t.Fatalf("save probe for %q: %v", name, err)
	}
}

func TestDefinitionsOrdersServersThenTools(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// Created out of alphabetical order to prove Definitions orders by
	// server name, not creation order.
	mustCreateServer(t, s, store.MCPServer{Name: "zeta", Transport: store.MCPTransportStdio, Command: "zeta-mcp", Enabled: true})
	mustCreateServer(t, s, store.MCPServer{Name: "alpha", Transport: store.MCPTransportStdio, Command: "alpha-mcp", Enabled: true})

	mustSaveProbe(t, s, "zeta", []store.MCPToolSnapshot{
		{Name: "zeta_two", QualifiedName: "mcp__zeta__zeta_two", Description: "second"},
		{Name: "zeta_one", QualifiedName: "mcp__zeta__zeta_one", Description: "first"},
	})
	mustSaveProbe(t, s, "alpha", []store.MCPToolSnapshot{
		{Name: "alpha_tool", QualifiedName: "mcp__alpha__alpha_tool", Description: "the one alpha tool"},
	})

	m := New(s)
	tools, _, err := m.Definitions(ctx)
	if err != nil {
		t.Fatalf("Definitions: %v", err)
	}

	wantOrder := []string{"mcp__alpha__alpha_tool", "mcp__zeta__zeta_two", "mcp__zeta__zeta_one"}
	if len(tools) != len(wantOrder) {
		t.Fatalf("got %d tools, want %d: %+v", len(tools), len(wantOrder), tools)
	}
	for i, name := range wantOrder {
		if tools[i].Function.Name != name {
			t.Fatalf("tool %d = %q, want %q (full: %+v)", i, tools[i].Function.Name, name, tools)
		}
		if tools[i].Type != "function" {
			t.Fatalf("tool %d Type = %q, want %q", i, tools[i].Type, "function")
		}
	}
}

func TestDefinitionsReadOnlyMap(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	mustCreateServer(t, s, store.MCPServer{Name: "readable", Transport: store.MCPTransportStdio, Command: "x", Enabled: true, AllowReadOnly: true})
	mustCreateServer(t, s, store.MCPServer{Name: "writable-only", Transport: store.MCPTransportStdio, Command: "x", Enabled: true, AllowReadOnly: false})

	m := New(s)
	_, readOnly, err := m.Definitions(ctx)
	if err != nil {
		t.Fatalf("Definitions: %v", err)
	}
	if !readOnly["readable"] {
		t.Fatalf("expected readable to be allowed under readonly, got %+v", readOnly)
	}
	if readOnly["writable-only"] {
		t.Fatalf("expected writable-only to be denied under readonly, got %+v", readOnly)
	}
}

func TestDefinitionsReadOnlyMapCoversServerWithNoTools(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// Enabled but never successfully probed: no tools in the snapshot, but
	// the permission gate still needs to know its allow_readonly value.
	mustCreateServer(t, s, store.MCPServer{Name: "unprobed", Transport: store.MCPTransportStdio, Command: "x", Enabled: true, AllowReadOnly: true})

	m := New(s)
	toolDefs, readOnly, err := m.Definitions(ctx)
	if err != nil {
		t.Fatalf("Definitions: %v", err)
	}
	if len(toolDefs) != 0 {
		t.Fatalf("expected no tool definitions from an unprobed server, got %+v", toolDefs)
	}
	if v, ok := readOnly["unprobed"]; !ok || !v {
		t.Fatalf("expected unprobed to appear in the read-only map as true, got %+v", readOnly)
	}
}

func TestDefinitionsEmptySchemaFallback(t *testing.T) {
	// A schema stored via SaveMCPProbe is always syntactically valid JSON —
	// json.RawMessage's own marshalling refuses anything else — so the
	// round trip only ever exercises the "empty" half of the fallback; the
	// "unparseable" half is exercised directly against toolParameters
	// below, since that is the only way that case can occur (stored data
	// this package itself did not write, e.g. through a hand-edited row).
	s := openTestStore(t)
	ctx := context.Background()

	mustCreateServer(t, s, store.MCPServer{Name: "srv", Transport: store.MCPTransportStdio, Command: "x", Enabled: true})
	mustSaveProbe(t, s, "srv", []store.MCPToolSnapshot{
		{Name: "no_schema", QualifiedName: "mcp__srv__no_schema", Description: "d"}, // InputSchema nil
		{Name: "good_schema", QualifiedName: "mcp__srv__good_schema", Description: "d", InputSchema: json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`)},
	})

	m := New(s)
	toolDefs, _, err := m.Definitions(ctx)
	if err != nil {
		t.Fatalf("Definitions: %v", err)
	}
	if len(toolDefs) != 2 {
		t.Fatalf("expected 2 tools, got %+v", toolDefs)
	}
	for _, td := range toolDefs {
		switch td.Function.Name {
		case "mcp__srv__no_schema":
			if string(td.Function.Parameters) != string(emptyObjectSchema) {
				t.Fatalf("%s: Parameters = %s, want the empty-object fallback", td.Function.Name, td.Function.Parameters)
			}
		case "mcp__srv__good_schema":
			if string(td.Function.Parameters) == string(emptyObjectSchema) {
				t.Fatalf("%s: expected the real schema to survive, got the fallback", td.Function.Name)
			}
		}
	}
}

func TestToolParametersFallsBackOnUnparseableSchema(t *testing.T) {
	got := toolParameters(json.RawMessage(`{not json`))
	if string(got) != string(emptyObjectSchema) {
		t.Fatalf("toolParameters(unparseable) = %s, want the empty-object fallback", got)
	}
}

func TestToolParametersFallsBackOnEmptySchema(t *testing.T) {
	got := toolParameters(nil)
	if string(got) != string(emptyObjectSchema) {
		t.Fatalf("toolParameters(nil) = %s, want the empty-object fallback", got)
	}
	got = toolParameters(json.RawMessage(``))
	if string(got) != string(emptyObjectSchema) {
		t.Fatalf("toolParameters(empty) = %s, want the empty-object fallback", got)
	}
}

func TestToolParametersPassesThroughValidSchema(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`)
	got := toolParameters(schema)
	if string(got) != string(schema) {
		t.Fatalf("toolParameters(valid) = %s, want unchanged %s", got, schema)
	}
}

func TestDefinitionsEmptyDescriptionSynthesised(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	mustCreateServer(t, s, store.MCPServer{Name: "srv", Transport: store.MCPTransportStdio, Command: "x", Enabled: true})
	mustSaveProbe(t, s, "srv", []store.MCPToolSnapshot{
		{Name: "nameless", QualifiedName: "mcp__srv__nameless", Description: ""},
	})

	m := New(s)
	toolDefs, _, err := m.Definitions(ctx)
	if err != nil {
		t.Fatalf("Definitions: %v", err)
	}
	if len(toolDefs) != 1 {
		t.Fatalf("expected 1 tool, got %+v", toolDefs)
	}
	if toolDefs[0].Function.Description == "" {
		t.Fatalf("expected a synthesised, non-empty description")
	}
}

func TestDefinitionsSkipsDisabledServer(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	mustCreateServer(t, s, store.MCPServer{Name: "off", Transport: store.MCPTransportStdio, Command: "x", Enabled: false})
	mustSaveProbe(t, s, "off", []store.MCPToolSnapshot{
		{Name: "t", QualifiedName: "mcp__off__t", Description: "d"},
	})

	m := New(s)
	toolDefs, readOnly, err := m.Definitions(ctx)
	if err != nil {
		t.Fatalf("Definitions: %v", err)
	}
	if len(toolDefs) != 0 {
		t.Fatalf("expected a disabled server to contribute nothing, got %+v", toolDefs)
	}
	if _, ok := readOnly["off"]; ok {
		t.Fatalf("expected a disabled server absent from the read-only map, got %+v", readOnly)
	}
}

func TestDefinitionsServerWithFailedProbeStillContributesLastSnapshot(t *testing.T) {
	// This is the invariant the whole design rests on (docs/MCP.md, "The
	// tool array is built from a stored snapshot, never from a live
	// connection"): a server that is enabled but currently unreachable
	// contributes whatever it contributed last time.
	s := openTestStore(t)
	ctx := context.Background()

	mustCreateServer(t, s, store.MCPServer{Name: "flaky", Transport: store.MCPTransportStdio, Command: "x", Enabled: true})
	mustSaveProbe(t, s, "flaky", []store.MCPToolSnapshot{
		{Name: "still_here", QualifiedName: "mcp__flaky__still_here", Description: "d"},
	})
	if err := s.SaveMCPProbe(ctx, "flaky", nil, "dial tcp: connection refused", time.Now().UTC()); err != nil {
		t.Fatalf("save failing probe: %v", err)
	}

	m := New(s)
	toolDefs, _, err := m.Definitions(ctx)
	if err != nil {
		t.Fatalf("Definitions: %v", err)
	}
	if len(toolDefs) != 1 || toolDefs[0].Function.Name != "mcp__flaky__still_here" {
		t.Fatalf("expected the surviving snapshot's tool, got %+v", toolDefs)
	}
}
