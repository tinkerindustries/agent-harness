package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrgeoffrich/deepseek-harness/internal/hub"
	"github.com/mrgeoffrich/deepseek-harness/internal/redact"
	"github.com/mrgeoffrich/deepseek-harness/internal/settings"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

// fakeMCPProber stands in for *mcpclient.Manager: Refresh records the call
// and drives the same store.SaveMCPProbe path a real probe would, so its
// behaviour matches the asymmetry docs/MCP.md describes — success writes a
// snapshot and clears probe_error, failure writes probe_error only — without
// this package depending on internal/mcpclient (which another agent is
// concurrently writing).
type fakeMCPProber struct {
	st *store.Store

	mu    sync.Mutex
	calls []string
	// fail, when set for a server name, makes Refresh record that message as
	// a failed probe instead of a successful one.
	fail map[string]string
	// tools is the snapshot a successful probe writes.
	tools []store.MCPToolSnapshot
	// completions is what Complete answers with, and completeErr makes it
	// fail the way a server that does not support completion would.
	completions   []string
	completeErr   error
	completeCalls []string
}

func (f *fakeMCPProber) Complete(_ context.Context, server, kind, ref, argName, argValue string) ([]string, error) {
	f.mu.Lock()
	f.completeCalls = append(f.completeCalls, strings.Join([]string{server, kind, ref, argName, argValue}, "|"))
	f.mu.Unlock()
	if f.completeErr != nil {
		return nil, f.completeErr
	}
	if f.completions == nil {
		return []string{}, nil
	}
	return f.completions, nil
}

func (f *fakeMCPProber) Refresh(ctx context.Context, name string) (store.MCPServer, error) {
	f.mu.Lock()
	f.calls = append(f.calls, name)
	errMsg := f.fail[name]
	f.mu.Unlock()

	if errMsg != "" {
		if err := f.st.SaveMCPProbe(ctx, name, store.MCPProbe{}, errMsg, time.Now()); err != nil {
			return store.MCPServer{}, err
		}
		// The reloaded row *and* the error, which is what
		// mcpclient.Manager.Refresh returns for a failed probe. Returning
		// a nil error here instead made this double disagree with the
		// contract it stands in for, and every handler test that exercised
		// a probe failure passed while the handler was answering 500 to
		// the real thing.
		row, err := f.st.GetMCPServer(ctx, name)
		if err != nil {
			return store.MCPServer{}, err
		}
		return row, errors.New(errMsg)
	}
	if err := f.st.SaveMCPProbe(ctx, name, store.MCPProbe{Tools: f.tools}, "", time.Now()); err != nil {
		return store.MCPServer{}, err
	}
	return f.st.GetMCPServer(ctx, name)
}

func (f *fakeMCPProber) callCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == name {
			n++
		}
	}
	return n
}

// newMCPTestServer is newTestServer plus an MCPProber, which the shared
// helper has no field to inject.
func newMCPTestServer(t *testing.T, prober MCPProber) (*httptest.Server, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "harness.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	api := &Server{
		Store: st, Hub: hub.New(), Settings: settings.NewResolver(st), MCP: prober,
		Static: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "static placeholder")
		}),
	}
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return srv, st
}

func mcpPost(t *testing.T, srv *httptest.Server, path, body string) *http.Response {
	t.Helper()
	return doWrite(t, srv, http.MethodPost, path, body, map[string]string{"Content-Type": "application/json"})
}

func mcpPatch(t *testing.T, srv *httptest.Server, path, body string) *http.Response {
	t.Helper()
	return doWrite(t, srv, http.MethodPatch, path, body, map[string]string{"Content-Type": "application/json"})
}

func mcpDelete(t *testing.T, srv *httptest.Server, path string) *http.Response {
	t.Helper()
	return doWrite(t, srv, http.MethodDelete, path, "", map[string]string{"Content-Type": "application/json"})
}

func decodeMCPWire(t *testing.T, resp *http.Response) mcpServerWire {
	t.Helper()
	defer resp.Body.Close()
	var w mcpServerWire
	if err := json.NewDecoder(resp.Body).Decode(&w); err != nil {
		t.Fatalf("decode mcp server wire: %v", err)
	}
	return w
}

// --- CRUD round trip ---

func TestMCPServerCRUDRoundTrip(t *testing.T) {
	srv, st := newMCPTestServer(t, nil)

	// List starts empty, and is an array, not null.
	resp, err := http.Get(srv.URL + "/api/mcp/servers")
	if err != nil {
		t.Fatal(err)
	}
	body := decodeArray(t, resp)
	if body != "[]" {
		t.Fatalf("empty list = %q, want []", body)
	}

	// Create with the enabled/allow_readonly defaults omitted.
	create := `{"name":"blender","transport":"stdio","command":"uvx","args":["blender-mcp"]}`
	resp = mcpPost(t, srv, "/api/mcp/servers", create)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	created := decodeMCPWire(t, resp)
	if created.Name != "blender" || created.Transport != "stdio" || created.Command != "uvx" {
		t.Fatalf("created row = %+v", created)
	}
	if !created.Enabled {
		t.Errorf("enabled default = false, want true")
	}
	if created.AllowReadOnly {
		t.Errorf("allow_readonly default = true, want false")
	}
	if created.ToolCount != 0 || len(created.Tools) != 0 || created.ProbedAt != "" {
		t.Errorf("a create with no prober configured must record no probe, got %+v", created)
	}

	// List now has one row.
	resp, err = http.Get(srv.URL + "/api/mcp/servers")
	if err != nil {
		t.Fatal(err)
	}
	var list []mcpServerWire
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(list) != 1 || list[0].Name != "blender" {
		t.Fatalf("list = %+v", list)
	}

	// Patch allow_readonly.
	resp = mcpPatch(t, srv, "/api/mcp/servers/blender", `{"allow_readonly":true}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch status = %d, want 200", resp.StatusCode)
	}
	patched := decodeMCPWire(t, resp)
	if !patched.AllowReadOnly {
		t.Errorf("allow_readonly after patch = false, want true")
	}

	// Delete.
	resp = mcpDelete(t, srv, "/api/mcp/servers/blender")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", resp.StatusCode)
	}
	resp.Body.Close()

	// Gone from the list, and gone from the store.
	resp, err = http.Get(srv.URL + "/api/mcp/servers")
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeArray(t, resp); got != "[]" {
		t.Fatalf("list after delete = %q, want []", got)
	}
	if _, err := st.GetMCPServer(context.Background(), "blender"); err == nil {
		t.Fatalf("row survived delete")
	}
}

func decodeArray(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var raw json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(raw))
}

// --- masking ---

func TestMCPServerMasksSecretsOnRead(t *testing.T) {
	srv, st := newMCPTestServer(t, nil)

	const secret = "sk-abcdefghijklmnop1234"
	create := fmt.Sprintf(`{"name":"blender","transport":"stdio","command":"uvx","env":{"API_KEY":%q}}`, secret)
	resp := mcpPost(t, srv, "/api/mcp/servers", create)
	created := decodeMCPWire(t, resp)
	if created.Env["API_KEY"] == secret {
		t.Fatalf("secret value not masked on create response: %q", created.Env["API_KEY"])
	}
	if want := redact.Secret(secret); created.Env["API_KEY"] != want {
		t.Errorf("masked value = %q, want %q", created.Env["API_KEY"], want)
	}

	// GET (list) also masks it, and the raw bytes never contain the secret.
	resp, err := http.Get(srv.URL + "/api/mcp/servers")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf strings.Builder
	if _, err := io.Copy(&buf, resp.Body); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), secret) {
		t.Fatalf("GET body leaked the stored secret: %s", buf.String())
	}

	// The stored value, read directly through the store, is the real one.
	stored, err := st.GetMCPServer(context.Background(), "blender")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Env["API_KEY"] != secret {
		t.Fatalf("stored env value = %q, want the unmasked secret", stored.Env["API_KEY"])
	}
}

// --- empty-value-keeps-stored and absent-key-removes ---

func TestMCPServerPatchEnvEmptyKeepsStoredAndAbsentRemoves(t *testing.T) {
	srv, st := newMCPTestServer(t, nil)

	create := `{"name":"blender","transport":"stdio","command":"uvx","env":{"A":"secret-a-value","B":"secret-b-value"}}`
	resp := mcpPost(t, srv, "/api/mcp/servers", create)
	resp.Body.Close()

	// A sent empty keeps the stored value; B is simply absent from the body
	// and is removed; C is new.
	resp = mcpPatch(t, srv, "/api/mcp/servers/blender", `{"env":{"A":"","C":"new-c-value"}}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	stored, err := st.GetMCPServer(context.Background(), "blender")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"A": "secret-a-value", "C": "new-c-value"}
	if len(stored.Env) != len(want) || stored.Env["A"] != want["A"] || stored.Env["C"] != want["C"] {
		t.Fatalf("stored env after patch = %+v, want %+v", stored.Env, want)
	}
	if _, ok := stored.Env["B"]; ok {
		t.Fatalf("key B survived being omitted from the patch body: %+v", stored.Env)
	}
}

// --- errors ---

func TestMCPServerCreateDuplicateIs409(t *testing.T) {
	srv, _ := newMCPTestServer(t, nil)
	create := `{"name":"blender","transport":"stdio","command":"uvx"}`
	resp := mcpPost(t, srv, "/api/mcp/servers", create)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first create status = %d, want 201", resp.StatusCode)
	}
	resp = mcpPost(t, srv, "/api/mcp/servers", create)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate create status = %d, want 409", resp.StatusCode)
	}
}

func TestMCPServer404s(t *testing.T) {
	prober := &fakeMCPProber{}
	srv, st := newMCPTestServer(t, prober)
	prober.st = st

	resp := mcpPatch(t, srv, "/api/mcp/servers/does-not-exist", `{"allow_readonly":true}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("patch unknown status = %d, want 404", resp.StatusCode)
	}

	resp = mcpDelete(t, srv, "/api/mcp/servers/does-not-exist")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("delete unknown status = %d, want 404", resp.StatusCode)
	}

	resp = mcpPost(t, srv, "/api/mcp/servers/does-not-exist/refresh", `{}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("refresh unknown status = %d, want 404", resp.StatusCode)
	}
}

func TestMCPServerBadNameIs400(t *testing.T) {
	srv, _ := newMCPTestServer(t, nil)
	resp := mcpPost(t, srv, "/api/mcp/servers", `{"name":"Not A Valid Name!","transport":"stdio","command":"uvx"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad name status = %d, want 400", resp.StatusCode)
	}
}

func TestMCPServerStdioWithURLIs400(t *testing.T) {
	srv, _ := newMCPTestServer(t, nil)
	resp := mcpPost(t, srv, "/api/mcp/servers", `{"name":"blender","transport":"stdio","command":"uvx","url":"https://example.com"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("stdio+url status = %d, want 400", resp.StatusCode)
	}
}

// --- refresh / probing ---

func TestMCPServerRefreshWithNilProberIs503(t *testing.T) {
	srv, _ := newMCPTestServer(t, nil)
	create := `{"name":"blender","transport":"stdio","command":"uvx"}`
	resp := mcpPost(t, srv, "/api/mcp/servers", create)
	resp.Body.Close()

	resp = mcpPost(t, srv, "/api/mcp/servers/blender/refresh", `{}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("refresh with no prober status = %d, want 503", resp.StatusCode)
	}
}

func TestMCPServerRefreshReturns200OnProbeFailure(t *testing.T) {
	prober := &fakeMCPProber{fail: map[string]string{"blender": "connection refused"}}
	srv, st := newMCPTestServer(t, prober)
	prober.st = st

	create := `{"name":"blender","transport":"stdio","command":"uvx"}`
	resp := mcpPost(t, srv, "/api/mcp/servers", create)
	resp.Body.Close()

	resp = mcpPost(t, srv, "/api/mcp/servers/blender/refresh", `{}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("refresh after a failed probe status = %d, want 200: a probe that could not "+
			"connect is reported on the row, never as a 5xx", resp.StatusCode)
	}
	row := decodeMCPWire(t, resp)
	if row.ProbeError != "connection refused" {
		t.Fatalf("probe_error = %q, want the recorded failure", row.ProbeError)
	}
	if row.Name != "blender" {
		t.Fatalf("row name = %q, want the server the probe failed for", row.Name)
	}
}

// TestMCPServerCreateReportsProbeFailureOnTheRow pins the other half of the
// same contract: a create whose probe fails is still a 201, and the body
// carries the reason rather than an empty probe_error. It answered with an
// empty one while the row moments later held the failure, because the
// handler discarded the row Refresh returned and re-read instead.
func TestMCPServerCreateReportsProbeFailureOnTheRow(t *testing.T) {
	prober := &fakeMCPProber{fail: map[string]string{"blender": "dialling MCP server \"blender\" timed out"}}
	srv, st := newMCPTestServer(t, prober)
	prober.st = st

	resp := mcpPost(t, srv, "/api/mcp/servers", `{"name":"blender","transport":"stdio","command":"uvx"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 even when the probe fails", resp.StatusCode)
	}
	row := decodeMCPWire(t, resp)
	if row.ProbeError == "" {
		t.Fatal("create response carried an empty probe_error; the reason the probe failed must be on the row it returns")
	}
}

func TestMCPServerCreateProbesWhenConfigured(t *testing.T) {
	prober := &fakeMCPProber{tools: []store.MCPToolSnapshot{
		{Name: "get_objects_summary", QualifiedName: "mcp__blender__get_objects_summary", Description: "list objects"},
	}}
	srv, st := newMCPTestServer(t, prober)
	prober.st = st

	create := `{"name":"blender","transport":"stdio","command":"uvx"}`
	resp := mcpPost(t, srv, "/api/mcp/servers", create)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	row := decodeMCPWire(t, resp)
	if row.ToolCount != 1 || len(row.Tools) != 1 || row.Tools[0].QualifiedName != "mcp__blender__get_objects_summary" {
		t.Fatalf("create did not carry the probe's snapshot: %+v", row)
	}
	if row.ProbedAt == "" {
		t.Errorf("probed_at empty after a successful probe")
	}
	if prober.callCount("blender") != 1 {
		t.Errorf("probe calls for blender = %d, want 1", prober.callCount("blender"))
	}
}

// TestMCPServerCreateFailedProbeStillCreates pins the rule from the task
// spec: a failed probe is not a failed create. The row exists, carries the
// error, and the response is still 201.
func TestMCPServerCreateFailedProbeStillCreates(t *testing.T) {
	prober := &fakeMCPProber{fail: map[string]string{"blender": "dial tcp: connection refused"}}
	srv, st := newMCPTestServer(t, prober)
	prober.st = st

	create := `{"name":"blender","transport":"stdio","command":"uvx"}`
	resp := mcpPost(t, srv, "/api/mcp/servers", create)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create with a failing probe status = %d, want 201", resp.StatusCode)
	}
	row := decodeMCPWire(t, resp)
	if row.ProbeError != "dial tcp: connection refused" {
		t.Fatalf("probe_error = %q, want the recorded failure", row.ProbeError)
	}
	if _, err := st.GetMCPServer(context.Background(), "blender"); err != nil {
		t.Fatalf("row does not exist after a create whose probe failed: %v", err)
	}
}

func TestMCPServerPatchConnectionChangeProbesButAllowReadOnlyOnlyDoesNot(t *testing.T) {
	prober := &fakeMCPProber{}
	srv, st := newMCPTestServer(t, prober)
	prober.st = st

	create := `{"name":"blender","transport":"stdio","command":"uvx"}`
	resp := mcpPost(t, srv, "/api/mcp/servers", create)
	resp.Body.Close()
	if prober.callCount("blender") != 1 {
		t.Fatalf("create should have probed once, got %d", prober.callCount("blender"))
	}

	// allow_readonly-only patch: no reprobe.
	resp = mcpPatch(t, srv, "/api/mcp/servers/blender", `{"allow_readonly":true}`)
	resp.Body.Close()
	if prober.callCount("blender") != 1 {
		t.Errorf("an allow_readonly-only patch must not reprobe, calls = %d", prober.callCount("blender"))
	}

	// A connection-detail change: reprobe.
	resp = mcpPatch(t, srv, "/api/mcp/servers/blender", `{"command":"uv"}`)
	resp.Body.Close()
	if prober.callCount("blender") != 2 {
		t.Errorf("a connection-detail patch must reprobe, calls = %d", prober.callCount("blender"))
	}
}

func TestMCPServerPatchEnabledOnlyGoesThroughSetEnabledAndProbesOnlyWhenTurningOn(t *testing.T) {
	prober := &fakeMCPProber{}
	srv, st := newMCPTestServer(t, prober)
	prober.st = st

	create := `{"name":"blender","transport":"stdio","command":"uvx","enabled":false}`
	resp := mcpPost(t, srv, "/api/mcp/servers", create)
	resp.Body.Close()
	// Created disabled: still probed once, since probing happens on create
	// regardless of the enabled flag.
	if prober.callCount("blender") != 1 {
		t.Fatalf("create should have probed once, got %d", prober.callCount("blender"))
	}

	// Disabling (already false -> false is a no-op write, still enabled-only)
	// must not reprobe.
	resp = mcpPatch(t, srv, "/api/mcp/servers/blender", `{"enabled":false}`)
	resp.Body.Close()
	if prober.callCount("blender") != 1 {
		t.Errorf("an enabled:false patch must not reprobe, calls = %d", prober.callCount("blender"))
	}

	// Turning it on reprobes, and goes through SetMCPServerEnabled — proven
	// indirectly here by checking the stored configuration was untouched
	// (SetMCPServerEnabled never rewrites transport/command).
	resp = mcpPatch(t, srv, "/api/mcp/servers/blender", `{"enabled":true}`)
	row := decodeMCPWire(t, resp)
	if !row.Enabled {
		t.Fatalf("enabled after patch = false, want true")
	}
	if prober.callCount("blender") != 2 {
		t.Errorf("enabling must reprobe, calls = %d", prober.callCount("blender"))
	}
	stored, err := st.GetMCPServer(context.Background(), "blender")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Command != "uvx" {
		t.Errorf("enabling touched the stored command: %q", stored.Command)
	}
}

// --- method gate ---

// TestMCPServerMethodGate extends the method-gate coverage
// TestNonGetMethodsReturn405EverywhereExceptWriteRoutes gives every other
// resource: GET/HEAD pass everywhere, POST is allowed on the collection and
// on one server's refresh subresource, PATCH and DELETE are allowed on one
// server's own path, and every other method 405s with a correct Allow
// header — including the refresh predicate's ordering: PATCH and DELETE do
// not leak from /api/mcp/servers/{name} onto its /refresh subresource.
func TestMCPServerMethodGate(t *testing.T) {
	srv, _ := newMCPTestServer(t, nil)
	client := srv.Client()

	// The collection: POST is allowed, PUT and PATCH and DELETE are not.
	resp := mcpPost(t, srv, "/api/mcp/servers", `{"name":"blender","transport":"stdio","command":"uvx"}`)
	resp.Body.Close()
	if resp.StatusCode == http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/mcp/servers: gate refused a permitted write")
	}
	for _, m := range []string{http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions} {
		req, err := http.NewRequest(m, srv.URL+"/api/mcp/servers", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s /api/mcp/servers: %v", m, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s /api/mcp/servers: got status %d, want 405", m, resp.StatusCode)
		}
		if got := resp.Header.Get("Allow"); got != "GET, HEAD, POST" {
			t.Errorf("%s /api/mcp/servers: Allow = %q, want %q", m, got, "GET, HEAD, POST")
		}
	}

	// One server's own path: PATCH and DELETE allowed, POST is not (POST
	// there would collide with the collection's create semantics).
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodOptions} {
		req, err := http.NewRequest(m, srv.URL+"/api/mcp/servers/blender", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s /api/mcp/servers/blender: %v", m, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s /api/mcp/servers/blender: got status %d, want 405", m, resp.StatusCode)
		}
		if got := resp.Header.Get("Allow"); got != "GET, HEAD, PATCH, DELETE" {
			t.Errorf("%s /api/mcp/servers/blender: Allow = %q, want %q", m, got, "GET, HEAD, PATCH, DELETE")
		}
	}

	// The refresh subresource: POST allowed, PATCH and DELETE are not (the
	// ordering isMCPServerRefreshPath must win against isMCPServerPath).
	resp = mcpPost(t, srv, "/api/mcp/servers/blender/refresh", `{}`)
	resp.Body.Close()
	if resp.StatusCode == http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/mcp/servers/blender/refresh: gate refused a permitted write")
	}
	for _, m := range []string{http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions} {
		req, err := http.NewRequest(m, srv.URL+"/api/mcp/servers/blender/refresh", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s /api/mcp/servers/blender/refresh: %v", m, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s /api/mcp/servers/blender/refresh: got status %d, want 405", m, resp.StatusCode)
		}
		if got := resp.Header.Get("Allow"); got != "GET, HEAD, POST" {
			t.Errorf("%s /api/mcp/servers/blender/refresh: Allow = %q, want %q", m, got, "GET, HEAD, POST")
		}
	}
}

// completionFixture stands a server up with prober wired in and one MCP
// server registered, the shape all three completion tests need.
func completionFixture(t *testing.T, prober *fakeMCPProber) *httptest.Server {
	t.Helper()
	srv, st := newMCPTestServer(t, prober)
	prober.st = st
	resp := mcpPost(t, srv, "/api/mcp/servers",
		`{"name":"git","transport":"stdio","command":"git-mcp"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: status = %d", resp.StatusCode)
	}
	return srv
}

// TestCompleteMCPServerReturnsTheServersSuggestions covers the completion
// endpoint's happy path, including that the reference kind and the typed
// value both reach the server.
func TestCompleteMCPServerReturnsTheServersSuggestions(t *testing.T) {
	prober := &fakeMCPProber{completions: []string{"main", "master"}}
	srv := completionFixture(t, prober)

	resp := mcpPost(t, srv, "/api/mcp/servers/git/complete",
		`{"kind":"prompt","ref":"review","argument":"branch","value":"ma"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var got struct {
		Values []string `json:"values"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Values) != 2 || got.Values[0] != "main" {
		t.Fatalf("values = %v, want the server's suggestions", got.Values)
	}
	if len(prober.completeCalls) != 1 || prober.completeCalls[0] != "git|prompt|review|branch|ma" {
		t.Fatalf("complete calls = %v, want the reference and the typed value carried through", prober.completeCalls)
	}
}

// TestCompleteMCPServerRejectsAnUnknownKind pins the one piece of the body
// with no sensible default: a reference resolved against the wrong list is
// not a mistake the server can report usefully.
func TestCompleteMCPServerRejectsAnUnknownKind(t *testing.T) {
	srv := completionFixture(t, &fakeMCPProber{})

	resp := mcpPost(t, srv, "/api/mcp/servers/git/complete",
		`{"kind":"neither","ref":"review","argument":"branch"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestCompleteMCPServerReportsAServerThatCannotComplete pins that a server
// without the capability produces a 502 naming it, not a 500 that reads
// like the harness broke.
func TestCompleteMCPServerReportsAServerThatCannotComplete(t *testing.T) {
	prober := &fakeMCPProber{completeErr: errors.New(`mcpclient: complete "branch" on "git": method not found`)}
	srv := completionFixture(t, prober)

	resp := mcpPost(t, srv, "/api/mcp/servers/git/complete",
		`{"kind":"prompt","ref":"review","argument":"branch"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "method not found") {
		t.Fatalf("body = %s, want the server's own reason", body)
	}
}
