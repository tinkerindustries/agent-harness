package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func testStdioServer(name string) MCPServer {
	return MCPServer{
		Name:      name,
		Transport: MCPTransportStdio,
		Command:   "blender-mcp",
		Args:      []string{"--flag", "value"},
		Env:       map[string]string{"API_KEY": "secret", "MODE": "prod"},
		Enabled:   true,
	}
}

// TestCreateAndGetMCPServer round-trips every field, including a multi-value
// env map and args slice.
func TestCreateAndGetMCPServer(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	srv := testStdioServer("blender")
	srv.AllowReadOnly = true
	if err := s.CreateMCPServer(ctx, srv); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := s.GetMCPServer(ctx, "blender")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "blender" || got.Transport != MCPTransportStdio || got.Command != "blender-mcp" {
		t.Fatalf("unexpected server: %+v", got)
	}
	if len(got.Args) != 2 || got.Args[0] != "--flag" || got.Args[1] != "value" {
		t.Fatalf("unexpected args: %+v", got.Args)
	}
	if len(got.Env) != 2 || got.Env["API_KEY"] != "secret" || got.Env["MODE"] != "prod" {
		t.Fatalf("unexpected env: %+v", got.Env)
	}
	if !got.Enabled || !got.AllowReadOnly {
		t.Fatalf("expected enabled and allow_readonly true, got %+v", got)
	}
	if got.URL != "" || len(got.Headers) != 0 {
		t.Fatalf("expected no url/headers for a stdio server, got %+v", got)
	}
	if len(got.Tools) != 0 {
		t.Fatalf("a never-probed server must carry no tools, got %+v", got.Tools)
	}
	if got.ProbedAt != "" || got.ProbeError != "" {
		t.Fatalf("a never-probed server must carry no probe state, got %+v", got)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatalf("expected created_at and updated_at set, got %+v", got)
	}
}

// TestCreateMCPServerHTTP round-trips an http-transport server.
func TestCreateMCPServerHTTP(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	srv := MCPServer{
		Name:      "search",
		Transport: MCPTransportHTTP,
		URL:       "https://mcp.example.com/search",
		Headers:   map[string]string{"Authorization": "Bearer tok"},
		Enabled:   true,
	}
	if err := s.CreateMCPServer(ctx, srv); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := s.GetMCPServer(ctx, "search")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.URL != srv.URL || got.Headers["Authorization"] != "Bearer tok" {
		t.Fatalf("unexpected server: %+v", got)
	}
	if got.Command != "" || len(got.Args) != 0 || len(got.Env) != 0 {
		t.Fatalf("expected no command/args/env for an http server, got %+v", got)
	}
}

// TestCreateMCPServerExists pins ErrMCPServerExists on a duplicate name.
func TestCreateMCPServerExists(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	srv := testStdioServer("blender")
	if err := s.CreateMCPServer(ctx, srv); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.CreateMCPServer(ctx, srv); !errors.Is(err, ErrMCPServerExists) {
		t.Fatalf("expected ErrMCPServerExists, got %v", err)
	}
}

// TestUnknownMCPServerNotFound pins ErrMCPServerNotFound on every write and
// read path when the name has no row.
func TestUnknownMCPServerNotFound(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if _, err := s.GetMCPServer(ctx, "ghost"); !errors.Is(err, ErrMCPServerNotFound) {
		t.Fatalf("Get: expected ErrMCPServerNotFound, got %v", err)
	}
	if err := s.UpdateMCPServer(ctx, testStdioServer("ghost")); !errors.Is(err, ErrMCPServerNotFound) {
		t.Fatalf("Update: expected ErrMCPServerNotFound, got %v", err)
	}
	if err := s.SetMCPServerEnabled(ctx, "ghost", false); !errors.Is(err, ErrMCPServerNotFound) {
		t.Fatalf("SetEnabled: expected ErrMCPServerNotFound, got %v", err)
	}
	if err := s.DeleteMCPServer(ctx, "ghost"); !errors.Is(err, ErrMCPServerNotFound) {
		t.Fatalf("Delete: expected ErrMCPServerNotFound, got %v", err)
	}
	if err := s.SaveMCPProbe(ctx, "ghost", nil, "", time.Now().UTC()); !errors.Is(err, ErrMCPServerNotFound) {
		t.Fatalf("SaveMCPProbe success: expected ErrMCPServerNotFound, got %v", err)
	}
	if err := s.SaveMCPProbe(ctx, "ghost", nil, "boom", time.Now().UTC()); !errors.Is(err, ErrMCPServerNotFound) {
		t.Fatalf("SaveMCPProbe failure: expected ErrMCPServerNotFound, got %v", err)
	}
}

// TestListMCPServersOrder pins name-ascending order and that
// ListEnabledMCPServers filters to enabled = 1.
func TestListMCPServersOrder(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	names := []string{"zebra", "alpha", "mid"}
	for _, n := range names {
		srv := testStdioServer(n)
		srv.Enabled = n != "mid"
		if err := s.CreateMCPServer(ctx, srv); err != nil {
			t.Fatalf("create %s: %v", n, err)
		}
	}

	all, err := s.ListMCPServers(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 servers, got %d", len(all))
	}
	got := []string{all[0].Name, all[1].Name, all[2].Name}
	want := []string{"alpha", "mid", "zebra"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ListMCPServers order = %v, want %v", got, want)
		}
	}

	enabled, err := s.ListEnabledMCPServers(ctx)
	if err != nil {
		t.Fatalf("list enabled: %v", err)
	}
	if len(enabled) != 2 {
		t.Fatalf("expected 2 enabled servers, got %d: %+v", len(enabled), enabled)
	}
	if enabled[0].Name != "alpha" || enabled[1].Name != "zebra" {
		t.Fatalf("ListEnabledMCPServers = %v, want [alpha zebra]", []string{enabled[0].Name, enabled[1].Name})
	}
}

// TestUpdateMCPServerLeavesProbeAndCreatedAt pins the invariant that a
// configuration edit is not a probe: UpdateMCPServer must leave the probe
// snapshot and created_at alone while moving updated_at forward.
func TestUpdateMCPServerLeavesProbeAndCreatedAt(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	srv := testStdioServer("blender")
	if err := s.CreateMCPServer(ctx, srv); err != nil {
		t.Fatalf("create: %v", err)
	}
	tools := []MCPToolSnapshot{{Name: "get_objects", Description: "list objects"}}
	probedAt := time.Now().UTC()
	if err := s.SaveMCPProbe(ctx, "blender", tools, "", probedAt); err != nil {
		t.Fatalf("probe: %v", err)
	}
	before, err := s.GetMCPServer(ctx, "blender")
	if err != nil {
		t.Fatalf("get before update: %v", err)
	}

	time.Sleep(2 * time.Millisecond) // ensure a distinguishable updated_at
	updated := before
	updated.Command = "blender-mcp-v2"
	updated.Args = []string{"--new-flag"}
	updated.Enabled = false
	if err := s.UpdateMCPServer(ctx, updated); err != nil {
		t.Fatalf("update: %v", err)
	}

	after, err := s.GetMCPServer(ctx, "blender")
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if after.Command != "blender-mcp-v2" || len(after.Args) != 1 || after.Args[0] != "--new-flag" {
		t.Fatalf("update did not apply: %+v", after)
	}
	if after.Enabled {
		t.Fatalf("expected enabled = false after update, got %+v", after)
	}
	if len(after.Tools) != 1 || after.Tools[0].Name != "get_objects" {
		t.Fatalf("update must leave the probe snapshot's tools alone, got %+v", after.Tools)
	}
	if after.ProbedAt != before.ProbedAt {
		t.Fatalf("update must leave probed_at alone: before %q, after %q", before.ProbedAt, after.ProbedAt)
	}
	if !after.CreatedAt.Equal(before.CreatedAt) {
		t.Fatalf("update must leave created_at alone: before %v, after %v", before.CreatedAt, after.CreatedAt)
	}
	if !after.UpdatedAt.After(before.UpdatedAt) {
		t.Fatalf("update must move updated_at forward: before %v, after %v", before.UpdatedAt, after.UpdatedAt)
	}
}

// TestSetMCPServerEnabledTouchesOnlyEnabled pins the toggle path: it flips
// enabled and moves updated_at, and nothing else on the row.
func TestSetMCPServerEnabledTouchesOnlyEnabled(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	srv := testStdioServer("blender")
	if err := s.CreateMCPServer(ctx, srv); err != nil {
		t.Fatalf("create: %v", err)
	}
	before, err := s.GetMCPServer(ctx, "blender")
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(2 * time.Millisecond)
	if err := s.SetMCPServerEnabled(ctx, "blender", false); err != nil {
		t.Fatalf("set enabled: %v", err)
	}
	after, err := s.GetMCPServer(ctx, "blender")
	if err != nil {
		t.Fatal(err)
	}
	if after.Enabled {
		t.Fatalf("expected enabled = false, got %+v", after)
	}
	if after.Command != before.Command || len(after.Args) != len(before.Args) {
		t.Fatalf("SetMCPServerEnabled must not touch configuration, got %+v", after)
	}
	if !after.UpdatedAt.After(before.UpdatedAt) {
		t.Fatalf("expected updated_at to move forward, before %v after %v", before.UpdatedAt, after.UpdatedAt)
	}
}

// TestDeleteMCPServer removes the row.
func TestDeleteMCPServer(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.CreateMCPServer(ctx, testStdioServer("blender")); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.DeleteMCPServer(ctx, "blender"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetMCPServer(ctx, "blender"); !errors.Is(err, ErrMCPServerNotFound) {
		t.Fatalf("expected ErrMCPServerNotFound after delete, got %v", err)
	}
}

// TestSaveMCPProbeSuccess pins the success half of the asymmetry: tools and
// probed_at are written and a previous error is cleared.
func TestSaveMCPProbeSuccess(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.CreateMCPServer(ctx, testStdioServer("blender")); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.SaveMCPProbe(ctx, "blender", nil, "connection refused", time.Now().UTC()); err != nil {
		t.Fatalf("seed failing probe: %v", err)
	}

	tools := []MCPToolSnapshot{
		{Name: "get_objects_summary", Description: "list scene objects", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}
	at := time.Now().UTC()
	if err := s.SaveMCPProbe(ctx, "blender", tools, "", at); err != nil {
		t.Fatalf("save success probe: %v", err)
	}

	got, err := s.GetMCPServer(ctx, "blender")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tools) != 1 || got.Tools[0].Name != "get_objects_summary" {
		t.Fatalf("expected the new tool snapshot, got %+v", got.Tools)
	}
	if got.ProbeError != "" {
		t.Fatalf("expected a successful probe to clear probe_error, got %q", got.ProbeError)
	}
	wantProbedAt := at.Format(time.RFC3339Nano)
	if got.ProbedAt != wantProbedAt {
		t.Fatalf("probed_at = %q, want %q", got.ProbedAt, wantProbedAt)
	}
}

// TestSaveMCPProbeFailureLeavesSnapshot is the invariant the whole design
// rests on (docs/MCP.md): a failed probe writes only the error and leaves
// the previous tools and probed_at exactly as they were.
func TestSaveMCPProbeFailureLeavesSnapshot(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.CreateMCPServer(ctx, testStdioServer("blender")); err != nil {
		t.Fatalf("create: %v", err)
	}
	tools := []MCPToolSnapshot{{Name: "get_objects_summary", Description: "list scene objects"}}
	at := time.Now().UTC()
	if err := s.SaveMCPProbe(ctx, "blender", tools, "", at); err != nil {
		t.Fatalf("seed successful probe: %v", err)
	}

	time.Sleep(2 * time.Millisecond)
	if err := s.SaveMCPProbe(ctx, "blender", nil, "dial tcp: connection refused", time.Now().UTC()); err != nil {
		t.Fatalf("save failing probe: %v", err)
	}

	got, err := s.GetMCPServer(ctx, "blender")
	if err != nil {
		t.Fatal(err)
	}
	if got.ProbeError != "dial tcp: connection refused" {
		t.Fatalf("expected the new probe_error, got %q", got.ProbeError)
	}
	if len(got.Tools) != 1 || got.Tools[0].Name != "get_objects_summary" {
		t.Fatalf("a failed probe must leave the previous tools intact, got %+v", got.Tools)
	}
	wantProbedAt := at.Format(time.RFC3339Nano)
	if got.ProbedAt != wantProbedAt {
		t.Fatalf("a failed probe must leave probed_at intact: got %q, want %q", got.ProbedAt, wantProbedAt)
	}
}

// TestMCPServerNilNormalisesToEmpty pins the decision that a round trip
// through the database never returns nil for Args, Env, Headers, or Tools —
// only the empty, non-nil shape — whatever the caller passed in.
func TestMCPServerNilNormalisesToEmpty(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	srv := MCPServer{
		Name:      "bare",
		Transport: MCPTransportStdio,
		Command:   "bare-mcp",
		// Args, Env deliberately left nil.
	}
	if err := s.CreateMCPServer(ctx, srv); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := s.GetMCPServer(ctx, "bare")
	if err != nil {
		t.Fatal(err)
	}
	if got.Args == nil {
		t.Fatal("expected Args to normalise to an empty, non-nil slice")
	}
	if got.Env == nil {
		t.Fatal("expected Env to normalise to an empty, non-nil map")
	}
	if got.Headers == nil {
		t.Fatal("expected Headers to normalise to an empty, non-nil map")
	}
	if got.Tools == nil {
		t.Fatal("expected Tools to normalise to an empty, non-nil slice")
	}
	if len(got.Args) != 0 || len(got.Env) != 0 || len(got.Headers) != 0 || len(got.Tools) != 0 {
		t.Fatalf("expected all four empty, got %+v", got)
	}
}

func TestValidateMCPServer(t *testing.T) {
	cases := []struct {
		name string
		srv  MCPServer
		want bool
	}{
		{
			name: "valid stdio",
			srv:  testStdioServer("blender"),
			want: true,
		},
		{
			name: "valid http",
			srv: MCPServer{
				Name:      "search",
				Transport: MCPTransportHTTP,
				URL:       "https://mcp.example.com/search",
				Headers:   map[string]string{"Authorization": "Bearer tok"},
			},
			want: true,
		},
		{
			name: "name uppercase",
			srv:  withName(testStdioServer("Blender"), "Blender"),
			want: false,
		},
		{
			name: "name leading hyphen",
			srv:  withName(testStdioServer("-blender"), "-blender"),
			want: false,
		},
		{
			name: "name too long",
			srv:  withName(testStdioServer("a"), "a-name-that-is-far-too-long-to-be-valid-here"),
			want: false,
		},
		{
			name: "name empty",
			srv:  withName(testStdioServer(""), ""),
			want: false,
		},
		{
			name: "unknown transport",
			srv: MCPServer{
				Name:      "blender",
				Transport: "carrier-pigeon",
				Command:   "blender-mcp",
			},
			want: false,
		},
		{
			name: "stdio with url",
			srv: MCPServer{
				Name:      "blender",
				Transport: MCPTransportStdio,
				Command:   "blender-mcp",
				URL:       "https://example.com",
			},
			want: false,
		},
		{
			name: "stdio missing command",
			srv: MCPServer{
				Name:      "blender",
				Transport: MCPTransportStdio,
			},
			want: false,
		},
		{
			name: "http without url",
			srv: MCPServer{
				Name:      "search",
				Transport: MCPTransportHTTP,
			},
			want: false,
		},
		{
			name: "http url not absolute",
			srv: MCPServer{
				Name:      "search",
				Transport: MCPTransportHTTP,
				URL:       "/relative/path",
			},
			want: false,
		},
		{
			name: "bad env key",
			srv: MCPServer{
				Name:      "blender",
				Transport: MCPTransportStdio,
				Command:   "blender-mcp",
				Env:       map[string]string{"1BAD": "x"},
			},
			want: false,
		},
		{
			name: "bad header key",
			srv: MCPServer{
				Name:      "search",
				Transport: MCPTransportHTTP,
				URL:       "https://mcp.example.com",
				Headers:   map[string]string{"Bad Header": "x"},
			},
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateMCPServer(c.srv)
			if (err == nil) != c.want {
				t.Errorf("ValidateMCPServer(%+v) error = %v, want error = %v", c.srv, err, !c.want)
			}
		})
	}
}

func withName(srv MCPServer, name string) MCPServer {
	srv.Name = name
	return srv
}
