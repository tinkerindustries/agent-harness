package mcpclient

import (
	"context"
	"fmt"
	"sort"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mrgeoffrich/agent-harness/internal/store"
	"github.com/mrgeoffrich/agent-harness/internal/tools"
)

// Resources and prompts are the two things a server advertises that do not
// become tools on the model's array (docs/MCP.md, "Resources", "Prompts").
// They are reached through four fixed built-in tools instead, and the
// reason is the frozen head: a session's tool array is resolved once and
// every request of that run carries the same bytes, so a server holding a
// thousand resources — a documentation set, a database's tables — would
// either flood the array or force it to change whenever the server's
// contents did. One tool that lists and one that reads costs four
// definitions no matter how much is behind them.
//
// Listing reads the stored snapshot, the same source the tool array is
// built from, so a server that is unreachable right now still lists what it
// had. Reading dials, because the contents are the point and a stale copy
// of them would be worse than an error.

// Resources implements tools.MCPProvider.
func (m *Manager) Resources(ctx context.Context) ([]tools.MCPResource, error) {
	servers, err := m.Store.ListEnabledMCPServers(ctx)
	if err != nil {
		return nil, err
	}
	var out []tools.MCPResource
	for _, srv := range servers {
		for _, r := range srv.Resources {
			out = append(out, tools.MCPResource{
				Server: srv.Name, URI: r.URI, Name: r.Name, Description: r.Description,
				MIMEType: r.MIMEType, Template: r.Template,
			})
		}
	}
	return out, nil
}

// Prompts implements tools.MCPProvider.
func (m *Manager) Prompts(ctx context.Context) ([]tools.MCPPrompt, error) {
	servers, err := m.Store.ListEnabledMCPServers(ctx)
	if err != nil {
		return nil, err
	}
	var out []tools.MCPPrompt
	for _, srv := range servers {
		for _, p := range srv.Prompts {
			args := make([]tools.MCPPromptArg, 0, len(p.Arguments))
			for _, a := range p.Arguments {
				args = append(args, tools.MCPPromptArg{Name: a.Name, Description: a.Description, Required: a.Required})
			}
			out = append(out, tools.MCPPrompt{
				Server: srv.Name, Name: p.Name, Description: p.Description, Arguments: args,
			})
		}
	}
	return out, nil
}

// ReadResource implements tools.MCPProvider: it dials server and reads uri.
//
// A template's URI is not accepted here, and not because reading one would
// fail — it might well succeed against a server that does not check. It is
// that `weather://forecast/{city}` read literally returns the server's
// answer for a city called "{city}", which is a plausible-looking wrong
// answer rather than an error, and the model has no way to tell.
func (m *Manager) ReadResource(ctx context.Context, server, uri string) (tools.MCPContent, error) {
	srv, err := m.enabledServer(ctx, server)
	if err != nil {
		return tools.MCPContent{}, err
	}
	if strings.ContainsAny(uri, "{}") {
		return tools.MCPContent{}, fmt.Errorf("mcpclient: %q is a resource template, not a resource: fill in its placeholders first", uri)
	}
	sess, err := m.session(ctx, srv)
	if err != nil {
		return tools.MCPContent{}, fmt.Errorf("mcpclient: connect to %q: %w", server, err)
	}
	res, err := sess.ReadResource(ctx, &mcpsdk.ReadResourceParams{URI: uri})
	if err != nil {
		return tools.MCPContent{}, fmt.Errorf("mcpclient: read %q from %q: %w", uri, server, err)
	}
	return flattenResource(res), nil
}

// GetPrompt implements tools.MCPProvider: it dials server and renders one
// prompt, flattening the messages it returns into text prefixed by role.
// The roles are kept because a prompt is a conversation a server composed —
// dropping them would run two speakers together into one block of text with
// nothing marking where the turn changed.
func (m *Manager) GetPrompt(ctx context.Context, server, name string, args map[string]string) (tools.MCPContent, error) {
	srv, err := m.enabledServer(ctx, server)
	if err != nil {
		return tools.MCPContent{}, err
	}
	sess, err := m.session(ctx, srv)
	if err != nil {
		return tools.MCPContent{}, fmt.Errorf("mcpclient: connect to %q: %w", server, err)
	}
	res, err := sess.GetPrompt(ctx, &mcpsdk.GetPromptParams{Name: name, Arguments: args})
	if err != nil {
		return tools.MCPContent{}, fmt.Errorf("mcpclient: get prompt %q from %q: %w", name, server, err)
	}

	var b strings.Builder
	if res.Description != "" {
		b.WriteString(res.Description)
		b.WriteString("\n\n")
	}
	for _, msg := range res.Messages {
		text := textOfContent([]mcpsdk.Content{msg.Content})
		if text == "" {
			continue
		}
		fmt.Fprintf(&b, "[%s] %s\n", msg.Role, text)
	}
	return tools.MCPContent{Text: strings.TrimRight(b.String(), "\n")}, nil
}

// Complete implements the argument-completion request (`completion/complete`)
// for the operator surface: given a prompt or resource template and the
// argument being typed, it returns the values the server suggests. Nothing
// in a run calls it — a model does not autocomplete — so it lives here for
// internal/httpapi rather than on the MCPProvider seam, and takes plain
// strings so that package needs no MCP types of its own.
//
// kind is "prompt" or "resource"; ref is the prompt's name or the resource
// template's URI. Always a non-nil slice on success, because "this server
// suggests nothing" and "this server was not asked" are different answers
// and a caller reading a nil could not tell them apart.
func (m *Manager) Complete(ctx context.Context, server, kind, ref, argName, argValue string) ([]string, error) {
	var reference *mcpsdk.CompleteReference
	switch kind {
	case "prompt":
		reference = &mcpsdk.CompleteReference{Type: "ref/prompt", Name: ref}
	case "resource":
		reference = &mcpsdk.CompleteReference{Type: "ref/resource", URI: ref}
	default:
		return nil, fmt.Errorf("mcpclient: unknown completion reference kind %q", kind)
	}
	srv, err := m.enabledServer(ctx, server)
	if err != nil {
		return nil, err
	}
	sess, err := m.session(ctx, srv)
	if err != nil {
		return nil, fmt.Errorf("mcpclient: connect to %q: %w", server, err)
	}
	res, err := sess.Complete(ctx, &mcpsdk.CompleteParams{
		Ref:      reference,
		Argument: mcpsdk.CompleteParamsArgument{Name: argName, Value: argValue},
	})
	if err != nil {
		return nil, fmt.Errorf("mcpclient: complete %q on %q: %w", argName, server, err)
	}
	if res.Completion.Values == nil {
		return []string{}, nil
	}
	return res.Completion.Values, nil
}

// enabledServer resolves a server name to its row, refusing one that is
// disabled — a disabled server contributes nothing to a session, and
// reaching it by name through a fixed tool would be a way around the
// toggle.
func (m *Manager) enabledServer(ctx context.Context, name string) (store.MCPServer, error) {
	servers, err := m.Store.ListEnabledMCPServers(ctx)
	if err != nil {
		return store.MCPServer{}, err
	}
	for _, srv := range servers {
		if srv.Name == name {
			return srv, nil
		}
	}
	known := make([]string, 0, len(servers))
	for _, srv := range servers {
		known = append(known, srv.Name)
	}
	sort.Strings(known)
	if len(known) == 0 {
		return store.MCPServer{}, fmt.Errorf("mcpclient: no MCP server named %q is enabled (none are)", name)
	}
	return store.MCPServer{}, fmt.Errorf("mcpclient: no MCP server named %q is enabled (enabled: %s)", name, strings.Join(known, ", "))
}

// flattenResource turns a resources/read reply into the text and images the
// executor knows how to place, the same vocabulary flatten uses for a tool
// call (content.go). A resource with binary contents that is not an image
// is named rather than decoded: its bytes are not something a model can
// read, and pasting base64 into the transcript would spend the context
// window saying nothing.
func flattenResource(res *mcpsdk.ReadResourceResult) tools.MCPContent {
	var out tools.MCPContent
	var text []string
	for _, c := range res.Contents {
		switch {
		case c.Text != "":
			text = append(text, c.Text)
		case len(c.Blob) > 0 && strings.HasPrefix(c.MIMEType, "image/"):
			out.Images = append(out.Images, tools.MCPImage{MIMEType: c.MIMEType, Data: c.Blob})
		case len(c.Blob) > 0:
			text = append(text, fmt.Sprintf("[%s: %d bytes of %s, not shown]", c.URI, len(c.Blob), c.MIMEType))
		}
	}
	out.Text = strings.Join(text, "\n")
	return out
}
