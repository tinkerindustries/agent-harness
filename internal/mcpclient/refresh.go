package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

// maxProbeErrorLen bounds how much of a probe failure SaveMCPProbe writes
// to probe_error, so a misbehaving server that dumps a stack trace or an
// HTML error page to its stderr or response body cannot bloat the row past
// what the /mcp screen has any use for.
const maxProbeErrorLen = 500

// maxInstructionsLen bounds how much of a server's initialize instructions
// SaveMCPProbe stores, and so how much of them can reach the opening
// message. The official Blender server's are around five kilobytes, which
// is the scale this is sized for: generous enough that no server writing
// instructions for a model to read gets clipped, small enough that a server
// echoing a manual into the field cannot quietly cost every session in the
// process thousands of prompt tokens on every request of every run.
const maxInstructionsLen = 32000

// probeWriteTimeout bounds recording a failed probe's reason. It is small
// because the write is local — one row in SQLite — and it exists only so
// the bookkeeping cannot itself hang forever on a context nothing will
// cancel.
const probeWriteTimeout = 10 * time.Second

// Refresh forces a fresh dial of name — bypassing the connection cache
// entirely, since the point of Refresh is to prove the server's *current*
// configuration actually connects, not that some earlier connection is
// still alive — lists its tools to the end of pagination, qualifies their
// names, reads the instructions the server sent at initialize, and records
// the outcome with SaveMCPProbe (docs/MCP.md, "Probing").
//
// On success it returns the freshly reloaded row. On failure it still
// returns the reloaded row, now carrying the new probe_error, alongside the
// error — so a caller (the /mcp screen, by way of internal/httpapi) can
// render both what went wrong and the row as it now stands, including
// whatever tool snapshot survived from the last successful probe
// (docs/MCP.md, "The tool array is built from a stored snapshot").
func (m *Manager) Refresh(ctx context.Context, name string) (store.MCPServer, error) {
	srv, err := m.Store.GetMCPServer(ctx, name)
	if err != nil {
		return store.MCPServer{}, err
	}

	snapshot, probeErr := m.probe(ctx, srv)
	if probeErr != nil {
		// Recording the failure runs on a fresh, bounded context rather
		// than ctx, the same rule Runner.FailSetup follows for a run's
		// terminal bookkeeping: a deadline is the most common reason a
		// probe fails, and ctx is then already expired, so writing the
		// reason through it fails too and the row keeps no account of why
		// — which is exactly what happened to a create whose probe timed
		// out and answered with an empty probe_error.
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), probeWriteTimeout)
		defer cancel()
		if err := m.Store.SaveMCPProbe(writeCtx, name, store.MCPProbe{}, truncateProbeError(probeErr), time.Now().UTC()); err != nil {
			return store.MCPServer{}, err
		}
		reloaded, err := m.Store.GetMCPServer(writeCtx, name)
		if err != nil {
			return store.MCPServer{}, err
		}
		return reloaded, probeErr
	}

	if err := m.Store.SaveMCPProbe(ctx, name, snapshot, "", time.Now().UTC()); err != nil {
		return store.MCPServer{}, err
	}
	return m.Store.GetMCPServer(ctx, name)
}

// probe dials srv fresh and reads everything it advertises: its tools to
// the end of pagination, with QualifiedName already computed — the one
// place that computation happens, so every reader of tools_json afterwards
// treats it as data rather than re-deriving it (internal/store's
// MCPToolSnapshot.QualifiedName doc comment) — its resources and resource
// templates, its prompts, and the instructions it sent in its
// InitializeResult.
//
// Each list is read only when the server's own capabilities say it has one.
// That is not an optimisation: a server that does not advertise resources
// answers resources/list with a method-not-found error, and a probe that
// asked anyway would turn every tools-only server — which is most of them,
// the official Blender server included — into a failed probe with a
// confusing reason.
//
// The instructions come from the handshake the dial already performed, so
// reading them costs no extra round trip: the SDK keeps the
// InitializeResult on the session. A server that sends none, and an SDK
// that has no result to give (a transport that seeded the session without
// a handshake), both yield "" — the same value a server has always
// effectively contributed here.
func (m *Manager) probe(ctx context.Context, srv store.MCPServer) (store.MCPProbe, error) {
	c, err := m.dial(ctx, srv)
	if err != nil {
		return store.MCPProbe{}, err
	}
	defer c.session.Close()
	sess := c.session

	snap := store.MCPProbe{Instructions: serverInstructions(sess)}

	var raw []*mcpsdk.Tool
	for tool, err := range sess.Tools(ctx, nil) {
		if err != nil {
			return store.MCPProbe{}, err
		}
		raw = append(raw, tool)
	}

	names := make([]string, len(raw))
	for i, t := range raw {
		names[i] = t.Name
	}
	qualified := QualifyToolNames(srv.Name, names)

	snap.Tools = make([]store.MCPToolSnapshot, len(raw))
	for i, t := range raw {
		schema, err := json.Marshal(t.InputSchema)
		if err != nil {
			return store.MCPProbe{}, fmt.Errorf("mcpclient: encode input schema for tool %q: %w", t.Name, err)
		}
		snap.Tools[i] = store.MCPToolSnapshot{
			Name:          t.Name,
			QualifiedName: qualified[i],
			Description:   t.Description,
			InputSchema:   schema,
		}
	}

	caps := sess.InitializeResult().Capabilities
	if caps != nil && caps.Resources != nil {
		for r, err := range sess.Resources(ctx, nil) {
			if err != nil {
				return store.MCPProbe{}, err
			}
			snap.Resources = append(snap.Resources, store.MCPResourceSnapshot{
				URI: r.URI, Name: r.Name, Description: r.Description, MIMEType: r.MIMEType,
			})
		}
		for t, err := range sess.ResourceTemplates(ctx, nil) {
			if err != nil {
				return store.MCPProbe{}, err
			}
			snap.Resources = append(snap.Resources, store.MCPResourceSnapshot{
				URI: t.URITemplate, Name: t.Name, Description: t.Description,
				MIMEType: t.MIMEType, Template: true,
			})
		}
	}

	if caps != nil && caps.Prompts != nil {
		for p, err := range sess.Prompts(ctx, nil) {
			if err != nil {
				return store.MCPProbe{}, err
			}
			args := make([]store.MCPPromptArgSnapshot, 0, len(p.Arguments))
			for _, a := range p.Arguments {
				args = append(args, store.MCPPromptArgSnapshot{
					Name: a.Name, Description: a.Description, Required: a.Required,
				})
			}
			snap.Prompts = append(snap.Prompts, store.MCPPromptSnapshot{
				Name: p.Name, Description: p.Description, Arguments: args,
			})
		}
	}

	return snap, nil
}

// serverInstructions returns the instructions sess's server sent at
// initialize, trimmed and capped at maxInstructionsLen. The nil check is
// not defensive padding: ClientSession.InitializeResult returns a pointer
// that is only populated once a handshake has completed, and a session
// seeded some other way would panic on a bare field read.
func serverInstructions(sess *mcpsdk.ClientSession) string {
	res := sess.InitializeResult()
	if res == nil {
		return ""
	}
	return truncateInstructions(strings.TrimSpace(res.Instructions))
}

// truncateInstructions cuts s to maxInstructionsLen bytes on a rune
// boundary, the same way truncateProbeError bounds a stored failure, and
// says so in the stored text: instructions that stop mid-sentence with no
// marker read to a model as a server that simply had nothing more to say.
func truncateInstructions(s string) string {
	if len(s) <= maxInstructionsLen {
		return s
	}
	b := []byte(s)[:maxInstructionsLen]
	for len(b) > 0 && !utf8.RuneStart(b[len(b)-1]) {
		b = b[:len(b)-1]
	}
	return string(b) + "\n… (truncated)"
}

// truncateProbeError renders err's message, cut to maxProbeErrorLen bytes
// on a rune boundary so the stored string stays valid UTF-8.
func truncateProbeError(err error) string {
	s := err.Error()
	if len(s) <= maxProbeErrorLen {
		return s
	}
	b := []byte(s)[:maxProbeErrorLen]
	for len(b) > 0 && !utf8.RuneStart(b[len(b)-1]) {
		b = b[:len(b)-1]
	}
	return string(b) + "… (truncated)"
}
