package stdiosession

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/mrgeoffrich/agent-harness/internal/mcpclient"
	"github.com/mrgeoffrich/agent-harness/internal/tools"
	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// HostServerName is the MCP server name a client's `function` tools are
// offered under. The loop reaches every tool that is not one of its own
// built-ins through the MCPProvider seam, and that seam addresses a tool by
// an "mcp__<server>__<tool>" name (internal/tools/mcp.go), so a function the
// client declared needs a server name to live under even though there is no
// server. This one is reserved: a client that also declares an `mcp_server`
// tool called "host" is refused.
const HostServerName = "host"

// hostTools implements tools.MCPProvider for one interaction. It merges two
// sources, which is the answer docs/STDIO-PROTOCOL.md argues for on whose
// tools a session gets:
//
//   - The `mcp_server` members of the create body's `tools` array, dialled by
//     internal/mcpclient over HTTP exactly as an operator-configured server
//     would be. This is Google's own tool type for the case, and it is the
//     path a parent with real tool servers should take: the tools arrive with
//     their own schemas, their results come back as MCP content, and nothing
//     new crosses the pipe.
//   - The `function` members, which have no server to dial and are called
//     back over the pipe with harness.function_call. This is for a parent
//     whose tools are in-process and which does not want to stand an HTTP
//     server up to expose them.
//
// The harness's own built-in tools (internal/tools) are always present and
// are not affected by either: they are the session's whole reason to be
// running on the parent's filesystem.
type hostTools struct {
	// mcp is the manager for the interaction's mcp_server tools, or nil
	// when the create body declared none.
	mcp tools.MCPProvider
	// allow is the set of server names this interaction declared, and names
	// the qualified tool names those servers advertised when they were
	// probed. The manager underneath is the process's own and answers for
	// every enabled row in the store, which for a state directory that has
	// hosted more than one session includes servers this interaction knows
	// nothing about — and a create that was refused still leaves its row
	// behind. Filtering here is what keeps an interaction's tools the ones
	// it asked for.
	//
	// names is an exact set taken from each declared server's own probe
	// rather than a prefix test on the qualified name. The two agree for
	// every name a current binary can write, since a server name may not
	// contain the "__" delimiter (internal/store, ValidateMCPServer), but
	// membership does not have to trust that — and a row an older binary
	// left in a state directory would not satisfy it.
	allow map[string]bool
	names map[string]bool

	mu    sync.Mutex
	funcs map[string]hostFunc
	// call sends one function call to the client and waits for its answer.
	call func(ctx context.Context, p FunctionCallParams) (FunctionCallResult, error)
	// responseID rides on every call so a client hosting more than one
	// session in one process knows which asked.
	responseID string
	readOnly   bool
}

// hostFunc is one client-declared function tool, keyed by the qualified name
// the model is offered.
type hostFunc struct {
	qualified   string
	name        string
	description string
	parameters  json.RawMessage
}

func newHostTools(mcp tools.MCPProvider, responseID string, call func(context.Context, FunctionCallParams) (FunctionCallResult, error)) *hostTools {
	return &hostTools{mcp: mcp, funcs: map[string]hostFunc{}, allow: map[string]bool{}, names: map[string]bool{}, call: call, responseID: responseID}
}

// addFunction registers one `function` tool from the create body.
func (h *hostTools) addFunction(t Tool) error {
	if t.Name == "" {
		return fmt.Errorf("a function tool needs a name")
	}
	qualified := mcpclient.QualifyToolName(HostServerName, t.Name)
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, clash := h.funcs[qualified]; clash {
		return fmt.Errorf("two function tools qualify to the same name %q", qualified)
	}
	params := t.Parameters
	if len(params) == 0 {
		params = json.RawMessage(`{"type":"object","properties":{}}`)
	}
	h.funcs[qualified] = hostFunc{qualified: qualified, name: t.Name, description: t.Description, parameters: params}
	// The permission seam is per namespace, not per tool
	// (tools.Policy.MCPReadOnlyServers), so one bool has to speak for every
	// function the client declared. It is an AND: the namespace counts as
	// read-only only when every function in it is. The other reading would
	// let one read-only tool carry a writing one into a readonly session,
	// which is the one mistake this must not make. A parent that wants its
	// read-only tools usable in a readonly session declares only those —
	// the rest would be denied there anyway.
	if len(h.funcs) == 1 {
		h.readOnly = t.Harness != nil && t.Harness.ReadOnly
	} else if t.Harness == nil || !t.Harness.ReadOnly {
		h.readOnly = false
	}
	return nil
}

// declaredTool reports whether a qualified name belongs to this
// interaction: one of the client's own functions, or a tool one of the
// servers it declared advertised at its probe. Both sides are exact sets.
func (h *hostTools) declaredTool(qualified string) bool {
	if _, ok := h.funcs[qualified]; ok {
		return true
	}
	return h.names[qualified]
}

func (h *hostTools) hasFunctions() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.funcs) > 0
}

// Definitions merges the dialled servers' tool array with the client's own
// function tools. The client's come last and in sorted order, so the array a
// session freezes is the same for the same create body — the byte stability
// the prompt cache depends on (docs/CACHE.md).
func (h *hostTools) Definitions(ctx context.Context) ([]wire.Tool, map[string]bool, error) {
	var defs []wire.Tool
	readOnly := map[string]bool{}
	if h.mcp != nil {
		d, ro, err := h.mcp.Definitions(ctx)
		if err != nil {
			return nil, nil, err
		}
		for _, t := range d {
			if h.declaredTool(t.Function.Name) {
				defs = append(defs, t)
			}
		}
		for k, v := range ro {
			if h.allow[k] {
				readOnly[k] = v
			}
		}
	}

	h.mu.Lock()
	names := make([]string, 0, len(h.funcs))
	for q := range h.funcs {
		names = append(names, q)
	}
	sort.Strings(names)
	for _, q := range names {
		f := h.funcs[q]
		defs = append(defs, wire.Tool{
			Type: "function",
			Function: wire.ToolFunction{
				Name:        f.qualified,
				Description: f.description,
				Parameters:  f.parameters,
			},
		})
	}
	if len(h.funcs) > 0 {
		readOnly[HostServerName] = h.readOnly
	}
	h.mu.Unlock()
	return defs, readOnly, nil
}

func (h *hostTools) Instructions(ctx context.Context) (map[string]string, error) {
	if h.mcp == nil {
		return nil, nil
	}
	all, err := h.mcp.Instructions(ctx)
	if err != nil || len(all) == 0 {
		return nil, err
	}
	out := make(map[string]string, len(all))
	for server, text := range all {
		if h.allow[server] {
			out[server] = text
		}
	}
	return out, nil
}

func (h *hostTools) Resources(ctx context.Context) ([]tools.MCPResource, error) {
	if h.mcp == nil {
		return nil, nil
	}
	all, err := h.mcp.Resources(ctx)
	if err != nil {
		return nil, err
	}
	var out []tools.MCPResource
	for _, r := range all {
		if h.allow[r.Server] {
			out = append(out, r)
		}
	}
	return out, nil
}

func (h *hostTools) ReadResource(ctx context.Context, server, uri string) (tools.MCPContent, error) {
	if h.mcp == nil || !h.allow[server] {
		return tools.MCPContent{}, fmt.Errorf("no MCP server named %q is configured for this interaction", server)
	}
	return h.mcp.ReadResource(ctx, server, uri)
}

func (h *hostTools) Prompts(ctx context.Context) ([]tools.MCPPrompt, error) {
	if h.mcp == nil {
		return nil, nil
	}
	all, err := h.mcp.Prompts(ctx)
	if err != nil {
		return nil, err
	}
	var out []tools.MCPPrompt
	for _, p := range all {
		if h.allow[p.Server] {
			out = append(out, p)
		}
	}
	return out, nil
}

func (h *hostTools) GetPrompt(ctx context.Context, server, name string, args map[string]string) (tools.MCPContent, error) {
	if h.mcp == nil || !h.allow[server] {
		return tools.MCPContent{}, fmt.Errorf("no MCP server named %q is configured for this interaction", server)
	}
	return h.mcp.GetPrompt(ctx, server, name, args)
}

// Call routes to the client over the pipe for a function tool, and to the
// dialled servers for everything else.
//
// A client that declines the request — because it does not implement
// harness.function_call, or because the tool is gone — gets that turned into
// an ordinary error tool result rather than a failed run. The model sees the
// message and can try something else, which is the same outcome as a tool
// that errored, and is why the protocol requires a decline rather than
// silence: an unanswered request would hold the sub-turn open until the tool
// timeout instead.
func (h *hostTools) Call(ctx context.Context, toolName string, args json.RawMessage) (tools.MCPContent, error) {
	h.mu.Lock()
	f, ok := h.funcs[toolName]
	call := h.call
	h.mu.Unlock()
	if !ok {
		if h.mcp == nil || !h.declaredTool(toolName) {
			return tools.MCPContent{}, fmt.Errorf("no MCP server serves %q", toolName)
		}
		return h.mcp.Call(ctx, toolName, args)
	}
	if call == nil {
		return tools.MCPContent{}, fmt.Errorf("this session cannot call client function %q: the client did not declare the function_calls capability", f.name)
	}
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	res, err := call(ctx, FunctionCallParams{
		ResponseID: h.responseID,
		Type:       ItemFunctionCall,
		// The call id is the one the client already saw on this call's
		// function_call item, so the answer it renders lands under the
		// right call (internal/tools, WithCallID).
		CallID:    tools.CallIDFrom(ctx),
		Name:      f.name,
		Arguments: args,
	})
	if err != nil {
		return tools.MCPContent{Text: fmt.Sprintf("the client could not run %s: %v", f.name, err), IsError: true}, nil
	}
	return contentFrom(res), nil
}

// contentFrom flattens a client's function_call_output into the MCPContent
// the executor turns into a tool result — the same shape internal/mcpclient
// produces from a real server's reply, so the executor cannot tell which
// path a result came back on.
func contentFrom(res FunctionCallResult) tools.MCPContent {
	out := tools.MCPContent{IsError: res.IsError}
	for _, c := range res.Output {
		switch c.Type {
		case PartInputText, PartOutputText, "text":
			if out.Text != "" {
				out.Text += "\n"
			}
			out.Text += c.Text
		case PartInputImage:
			// The surface carries an image as one base64 data URL rather
			// than a mime type beside a payload, so it is split here into
			// what the executor's own image type wants.
			mime, b64, ok := splitDataURI(c.ImageURL)
			if !ok {
				continue
			}
			data, err := decodeBase64(b64)
			if err != nil {
				continue
			}
			out.Images = append(out.Images, tools.MCPImage{MIMEType: mime, Data: data})
		}
	}
	return out
}

// splitDataURI splits a "data:<mime>;base64,<payload>" URL into its mime
// type and payload. The Responses surface carries an image as one such URL,
// where the executor's own image type wants the two separately.
func splitDataURI(uri string) (mime, payload string, ok bool) {
	const prefix = "data:"
	if !strings.HasPrefix(uri, prefix) {
		return "", "", false
	}
	rest := uri[len(prefix):]
	comma := strings.IndexByte(rest, ',')
	if comma < 0 {
		return "", "", false
	}
	header := rest[:comma]
	semi := strings.IndexByte(header, ';')
	if semi < 0 || header[semi+1:] != "base64" {
		return "", "", false
	}
	return header[:semi], rest[comma+1:], true
}
