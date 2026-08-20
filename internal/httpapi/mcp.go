package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/mrgeoffrich/deepseek-harness/internal/redact"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

// The MCP server registry (docs/MCP.md): GET /api/mcp/servers serves every
// configured server, masked; POST creates one; PATCH edits one; DELETE
// removes one; POST .../refresh probes one now. This is the write surface
// docs/MCP.md's "browser /mcp screen" box points at — the table it edits is
// mcp_servers, read and written entirely through internal/store's own API
// (ValidateMCPServer, ListMCPServers, GetMCPServer, CreateMCPServer,
// UpdateMCPServer, SetMCPServerEnabled, DeleteMCPServer). Probing itself
// never happens here: this file only ever calls the MCPProber seam
// (server.go), which cmd/harness wires to *mcpclient.Manager — this package
// holds no MCP client and starts no subprocess.

// mcpProbeTimeout bounds one probe this package asks for — on create, on a
// PATCH that changes connection details or enables a server, and on an
// explicit refresh. A probe that connects a subprocess or dials an HTTP MCP
// server can hang past any request timeout an operator would tolerate, so
// the context handed to MCPProber.Refresh is capped here rather than left to
// inherit whatever the request's own context allows.
const mcpProbeTimeout = 60 * time.Second

// mcpToolWire is one tool from the last successful probe, as served. Only
// what the screen renders: not InputSchema, which is large and unused by it
// (docs/MCP.md phase 4 scope).
type mcpToolWire struct {
	Name          string `json:"name"`
	QualifiedName string `json:"qualified_name"`
	Description   string `json:"description"`
}

// mcpServerWire is the wire shape of one mcp_servers row — the exact field
// set and order the frontend is built against. Env and Headers values are
// masked (maskMCPSecretMap); everything else is the row as stored.
type mcpServerWire struct {
	Name          string            `json:"name"`
	Transport     string            `json:"transport"`
	Command       string            `json:"command"`
	Args          []string          `json:"args"`
	Env           map[string]string `json:"env"`
	URL           string            `json:"url"`
	Headers       map[string]string `json:"headers"`
	Enabled       bool              `json:"enabled"`
	AllowReadOnly bool              `json:"allow_readonly"`
	Tools         []mcpToolWire     `json:"tools"`
	ToolCount     int               `json:"tool_count"`
	ProbedAt      string            `json:"probed_at"`
	ProbeError    string            `json:"probe_error"`
	CreatedAt     time.Time         `json:"created_at"`
	UpdatedAt     time.Time         `json:"updated_at"`
}

// mcpServerToWire projects a store row onto the wire shape, masking Env and
// Headers on the way out — the secret rule docs/MCP.md's "The table" section
// states: keys in the clear, values masked with redact.Secret. store.MCPServer
// guarantees Args, Env, Headers, and Tools are never nil, so the projection
// never turns an empty collection into a JSON null.
func mcpServerToWire(srv store.MCPServer) mcpServerWire {
	tools := make([]mcpToolWire, 0, len(srv.Tools))
	for _, t := range srv.Tools {
		tools = append(tools, mcpToolWire{Name: t.Name, QualifiedName: t.QualifiedName, Description: t.Description})
	}
	return mcpServerWire{
		Name: srv.Name, Transport: srv.Transport, Command: srv.Command,
		Args: srv.Args, Env: maskMCPSecretMap(srv.Env), URL: srv.URL,
		Headers: maskMCPSecretMap(srv.Headers), Enabled: srv.Enabled, AllowReadOnly: srv.AllowReadOnly,
		Tools: tools, ToolCount: len(srv.Tools), ProbedAt: srv.ProbedAt, ProbeError: srv.ProbeError,
		CreatedAt: srv.CreatedAt, UpdatedAt: srv.UpdatedAt,
	}
}

// maskMCPSecretMap masks every value of m with redact.Secret, keys
// untouched, the same masking settings.go's handleGetSettings applies to a
// secret value. It always returns a non-nil map so an empty Env or Headers
// serialises as {} rather than null.
func maskMCPSecretMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = redact.Secret(v)
	}
	return out
}

// mergeMCPSecretMap applies the write half of the same rule (docs/MCP.md
// "The table"): body is the caller's complete desired key set for Env or
// Headers, not a diff. A key present in body with an empty string keeps
// whatever value is already stored for that key — the only way an operator
// can edit a server's connection details without re-typing every API key
// GET only ever showed them masked. A key present with any other value
// overwrites the stored one. A key that exists in stored but does not
// appear in body at all is dropped: that is how a key is removed.
func mergeMCPSecretMap(stored, body map[string]string) map[string]string {
	merged := make(map[string]string, len(body))
	for k, v := range body {
		if v == "" {
			if old, ok := stored[k]; ok {
				merged[k] = old
				continue
			}
		}
		merged[k] = v
	}
	return merged
}

// probeIfConfigured re-probes name through s.MCP and returns the freshest
// row available. When s.MCP is nil, or Refresh itself returns a Go error —
// which docs/MCP.md's contract for MCPProber reserves for something outside
// probing, such as the row having vanished, since a failed *connection* is
// recorded on the row as probe_error rather than returned as an error — this
// falls back to a plain read, so an unconfigured or momentarily failing
// prober never turns an otherwise successful write into a failed one.
func (s *Server) probeIfConfigured(ctx context.Context, name string) (store.MCPServer, error) {
	if s.MCP != nil {
		pctx, cancel := context.WithTimeout(ctx, mcpProbeTimeout)
		defer cancel()
		if srv, err := s.MCP.Refresh(pctx, name); err == nil {
			return srv, nil
		}
	}
	return s.Store.GetMCPServer(ctx, name)
}

// writeMCPServerError maps a store error from an mcp_servers write to the
// wire: not found is 404, a duplicate name is 409, anything else is a 500
// like every other handler (docs/DATA-API.md "Error shape").
func writeMCPServerError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrMCPServerNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "mcp server not found"})
	case errors.Is(err, store.ErrMCPServerExists):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	default:
		writeInternalError(w, err)
	}
}

// handleListMCPServers serves GET /api/mcp/servers: every row in name order
// (store.ListMCPServers already orders it), masked. Always a JSON array,
// never null, even with zero rows configured.
func (s *Server) handleListMCPServers(w http.ResponseWriter, r *http.Request) {
	servers, err := s.Store.ListMCPServers(r.Context())
	if err != nil {
		writeInternalError(w, err)
		return
	}
	out := make([]mcpServerWire, 0, len(servers))
	for _, srv := range servers {
		out = append(out, mcpServerToWire(srv))
	}
	writeJSON(w, http.StatusOK, out)
}

// createMCPServerBody is the JSON body POST /api/mcp/servers accepts: the
// writable subset of a server. Enabled and AllowReadOnly are pointers so a
// body that omits them gets the documented defaults (true, false) rather
// than the zero value of bool silently meaning "off" for both.
type createMCPServerBody struct {
	Name          string            `json:"name"`
	Transport     string            `json:"transport"`
	Command       string            `json:"command"`
	Args          []string          `json:"args"`
	Env           map[string]string `json:"env"`
	URL           string            `json:"url"`
	Headers       map[string]string `json:"headers"`
	Enabled       *bool             `json:"enabled"`
	AllowReadOnly *bool             `json:"allow_readonly"`
}

// handleCreateMCPServer serves POST /api/mcp/servers: validates with
// store.ValidateMCPServer (400 on failure), creates the row (409 on
// store.ErrMCPServerExists), and, when s.MCP is wired, probes it once before
// answering. A failed probe is not a failed create — the row exists either
// way, the error lands on probe_error, and the response is 201 carrying
// whatever the freshest read of the row is (docs/MCP.md "Probing").
func (s *Server) handleCreateMCPServer(w http.ResponseWriter, r *http.Request) {
	if !writeGuards(w, r) {
		return
	}
	var body createMCPServerBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: expected an mcp server"})
		return
	}
	srv := store.MCPServer{
		Name: body.Name, Transport: body.Transport, Command: body.Command,
		Args: body.Args, Env: body.Env, URL: body.URL, Headers: body.Headers,
		Enabled: true, AllowReadOnly: false,
	}
	if body.Enabled != nil {
		srv.Enabled = *body.Enabled
	}
	if body.AllowReadOnly != nil {
		srv.AllowReadOnly = *body.AllowReadOnly
	}
	if err := store.ValidateMCPServer(srv); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.Store.CreateMCPServer(r.Context(), srv); err != nil {
		writeMCPServerError(w, err)
		return
	}
	result, err := s.probeIfConfigured(r.Context(), srv.Name)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, mcpServerToWire(result))
}

// patchMCPServerBody is the JSON body PATCH /api/mcp/servers/{name} accepts.
// Every field is a pointer: a field absent from the body is nil and keeps
// the row's stored value, which is what lets an operator flip one setting
// (say, allow_readonly) without resending the whole server. A struct of
// pointers is chosen over decoding into a map[string]json.RawMessage
// because every field here has a fixed, known shape (string, []string,
// map[string]string, bool) — nothing polymorphic that would need the map
// form's per-key type dispatch — so the struct keeps the same
// encoding/json.Decoder call every other handler in this package uses.
type patchMCPServerBody struct {
	Transport     *string            `json:"transport"`
	Command       *string            `json:"command"`
	Args          *[]string          `json:"args"`
	Env           *map[string]string `json:"env"`
	URL           *string            `json:"url"`
	Headers       *map[string]string `json:"headers"`
	Enabled       *bool              `json:"enabled"`
	AllowReadOnly *bool              `json:"allow_readonly"`
}

// handlePatchMCPServer serves PATCH /api/mcp/servers/{name}: a partial
// update of the row (404 when name is unknown, 400 on validation).
//
// A body that touches only Enabled goes through store.SetMCPServerEnabled
// rather than store.UpdateMCPServer — the toggle path that touches only
// enabled and updated_at, so it never disturbs the stored configuration or
// probe snapshot (internal/store/mcp.go). Any other body — one that changes
// a connection field, AllowReadOnly, or both — is applied as a merge onto
// the existing row and written with UpdateMCPServer.
//
// A re-probe follows the write only when connection details changed
// (transport, command, args, env, url, or headers present in the body) or
// the server was enabled by this request; toggling allow_readonly, or
// disabling a server, must not restart a subprocess.
func (s *Server) handlePatchMCPServer(w http.ResponseWriter, r *http.Request) {
	if !writeGuards(w, r) {
		return
	}
	name := r.PathValue("name")
	existing, err := s.Store.GetMCPServer(r.Context(), name)
	if err != nil {
		writeMCPServerError(w, err)
		return
	}
	var body patchMCPServerBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: expected a partial mcp server"})
		return
	}

	connectionTouched := body.Transport != nil || body.Command != nil || body.Args != nil ||
		body.Env != nil || body.URL != nil || body.Headers != nil
	onlyEnabledTouched := body.Enabled != nil && !connectionTouched && body.AllowReadOnly == nil
	reprobe := connectionTouched || (body.Enabled != nil && *body.Enabled)

	if onlyEnabledTouched {
		if err := s.Store.SetMCPServerEnabled(r.Context(), name, *body.Enabled); err != nil {
			writeMCPServerError(w, err)
			return
		}
	} else {
		merged := existing
		if body.Transport != nil {
			merged.Transport = *body.Transport
		}
		if body.Command != nil {
			merged.Command = *body.Command
		}
		if body.Args != nil {
			merged.Args = *body.Args
		}
		if body.URL != nil {
			merged.URL = *body.URL
		}
		if body.Env != nil {
			merged.Env = mergeMCPSecretMap(existing.Env, *body.Env)
		}
		if body.Headers != nil {
			merged.Headers = mergeMCPSecretMap(existing.Headers, *body.Headers)
		}
		if body.Enabled != nil {
			merged.Enabled = *body.Enabled
		}
		if body.AllowReadOnly != nil {
			merged.AllowReadOnly = *body.AllowReadOnly
		}
		if err := store.ValidateMCPServer(merged); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if err := s.Store.UpdateMCPServer(r.Context(), merged); err != nil {
			writeMCPServerError(w, err)
			return
		}
	}

	var result store.MCPServer
	if reprobe {
		result, err = s.probeIfConfigured(r.Context(), name)
	} else {
		result, err = s.Store.GetMCPServer(r.Context(), name)
	}
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, mcpServerToWire(result))
}

// handleDeleteMCPServer serves DELETE /api/mcp/servers/{name}: removes the
// row. 204 with no body on success, 404 when name is unknown.
func (s *Server) handleDeleteMCPServer(w http.ResponseWriter, r *http.Request) {
	if !writeGuards(w, r) {
		return
	}
	if err := s.Store.DeleteMCPServer(r.Context(), r.PathValue("name")); err != nil {
		writeMCPServerError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRefreshMCPServer serves POST /api/mcp/servers/{name}/refresh: probe
// now. 503 when s.MCP is nil — no client manager wired means no probe can
// happen, and that must fail closed, the same shape the missing run
// publisher takes for POST /api/runs. 404 when name is unknown. Otherwise
// 200 with the row, including when the probe itself failed: probe_error is
// on the row, and that is what the screen shows — a probe failure is never
// turned into a 5xx here (docs/MCP.md "Probing").
func (s *Server) handleRefreshMCPServer(w http.ResponseWriter, r *http.Request) {
	if !writeGuards(w, r) {
		return
	}
	if s.MCP == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "mcp probing is not configured: no client manager wired (start harness serve once)",
		})
		return
	}
	name := r.PathValue("name")
	ctx, cancel := context.WithTimeout(r.Context(), mcpProbeTimeout)
	defer cancel()
	srv, err := s.MCP.Refresh(ctx, name)
	if err != nil {
		writeMCPServerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, mcpServerToWire(srv))
}
