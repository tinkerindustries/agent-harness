package geministdio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/store"
	"github.com/mrgeoffrich/agent-harness/internal/tools"
	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// Resuming a session across a restart of this process.
//
// previous_interaction_id resolves against this process's own memory, so it
// reaches only interactions this process ran. harness.resume_session_id
// resolves against the state directory instead: the session row, its event
// log and its frozen tool array are all in the SQLite file under -state-dir,
// which outlives the process that wrote it. A parent that respawns
// `harness gemini-session` on the same -state-dir hands the new process a
// session id and gets the conversation back.
//
// What a resume must not do is move the session's prompt prefix. The system
// prompt and the tool array are the shared prefix every request of a session
// sends, and the prompt cache is built on them (docs/CACHE.md), so a resumed
// session sends the array it froze rather than one resolved fresh
// (internal/session, Resume). Everything here exists to make that honest: the
// create either reproduces the frozen shape and is accepted, or names a
// difference and is refused.

// resumeTarget resolves harness.resume_session_id against the store and
// checks that nothing the create names would change the session's frozen
// shape. It returns the session row, whose Workspace and Model the
// interaction inherits.
func (s *Server) resumeTarget(ctx context.Context, p CreateParams) (store.Session, *rpcError) {
	id := p.Harness.ResumeSessionID
	sess, err := s.opts.Store.GetSession(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return store.Session{}, errorf(CodeSessionNotFound, "no session %q under this process's state directory; a resume reads a log an earlier process wrote, so start this process with the -state-dir that session was recorded under", id)
	}
	if err != nil {
		return store.Session{}, errorf(CodeInternalError, "load session %s: %v", id, err)
	}

	// A row still marked running is one an earlier process died holding.
	// The state directory belongs to one process at a time — one process
	// hosts one session — so there is no other worker whose claim this
	// could be trampling, which is what makes reclaiming it safe here and
	// not in serve.
	if sess.Status == store.StatusRunning {
		if err := s.opts.Store.CancelRunningSession(ctx, id, time.Now().UTC()); err != nil {
			return store.Session{}, errorf(CodeInternalError, "reclaim session %s, which an earlier process left running: %v", id, err)
		}
		sess, err = s.opts.Store.GetSession(ctx, id)
		if err != nil {
			return store.Session{}, errorf(CodeInternalError, "reload session %s: %v", id, err)
		}
	}
	if sess.Status == store.StatusCreating {
		return store.Session{}, errorf(CodeInvalidParams, "session %s never got as far as a workspace, so there is no run to continue", id)
	}
	if sess.Status == store.StatusCompacted {
		return store.Session{}, errorf(CodeInvalidParams, "session %s was retired by compaction; resume the child session that replaced it", id)
	}

	if p.Model != "" && p.Model != sess.Model {
		return store.Session{}, errorf(CodeInvalidParams, "session %s ran on %s; a resumed session cannot change model, because its prompt prefix is frozen", id, sess.Model)
	}
	if p.Harness.CWD != "" && p.Harness.CWD != sess.Workspace {
		return store.Session{}, errorf(CodeInvalidParams, "session %s works in %s; a resumed session cannot change directory, and inherits the one it started in", id, sess.Workspace)
	}
	if p.Harness.PermissionMode != "" && p.Harness.PermissionMode != sess.PermissionMode {
		return store.Session{}, errorf(CodeInvalidParams, "session %s runs in %s permission mode; a resumed session keeps the mode its chain was started with", id, sess.PermissionMode)
	}
	if len(p.Harness.Deny) > 0 && !sameStrings(p.Harness.Deny, sess.DenyPatterns) {
		return store.Session{}, errorf(CodeInvalidParams, "session %s carries its own deny patterns (%s); a resumed session keeps them and cannot be given a different set", id, strings.Join(sess.DenyPatterns, ", "))
	}
	// The result schema is what the Complete tool validates the run's answer
	// against, and it is on the row for the same reason the rest of this is:
	// Resume reads it there rather than from the create.
	if p.ResponseFormat != nil && len(p.ResponseFormat.Schema) > 0 &&
		canonicalSchema(p.ResponseFormat.Schema) != canonicalSchema(sess.ResultSchema) {
		return store.Session{}, errorf(CodeInvalidParams, "session %s carries its own result schema; a resumed session keeps the one it was started with, and response_format cannot replace it", id)
	}
	return sess, nil
}

// frozenTools is the part of a session's stored tool array that a resuming
// create has to reproduce: everything the loop reaches through the MCP seam,
// which is the client's `mcp_server` tools and its `function` tools alike
// (internal/tools, MCPToolPrefix). The harness's own built-ins are not in
// here — they come from this binary and cannot differ between two processes
// running it.
type frozenTools struct {
	// schemas maps a qualified tool name to its parameter schema, in the
	// canonical encoding canonicalSchema produces.
	schemas map[string]string
}

// frozenToolsOf reads the mcp-namespaced entries out of a session's stored
// tool_schema column.
func frozenToolsOf(raw json.RawMessage) (frozenTools, error) {
	var all []wire.Tool
	if err := json.Unmarshal(raw, &all); err != nil {
		return frozenTools{}, err
	}
	f := frozenTools{schemas: map[string]string{}}
	for _, t := range all {
		name := t.Function.Name
		if !strings.HasPrefix(name, tools.MCPToolPrefix) {
			continue
		}
		f.schemas[name] = canonicalSchema(t.Function.Parameters)
	}
	return f, nil
}

// checkDeclarations matches the create's `mcp_server` declarations against
// the session's frozen tools, before any of them is written to the store or
// dialled. Both directions matter, and both are cheaper to answer here:
//
//   - A server the create forgot is the "fresh connection metadata" half of
//     a resume. The URL and headers a parent stood a server up on last time
//     went with the process that spawned it, so a resume supplies the ones
//     that are good now, and a parent that forgot one is told which rather
//     than watching its tools go missing.
//   - A server the create adds cannot contribute anything, because the run
//     sends the frozen array. Refusing it here is what keeps a rejected
//     create from leaving an enabled row behind for every later create to
//     trip over.
//
// Which server owns a frozen tool is decided by matching each declared
// server's prefix against the name, never by taking the name apart: a server
// name may itself contain "__" (internal/tools, MCPServerOf), and a create
// that named one would otherwise be unable to resume at all.
func checkDeclarations(f frozenTools, decls []Tool) *rpcError {
	var declared []string
	for _, t := range decls {
		if t.Type == ToolMCPServer && t.Name != "" {
			declared = append(declared, t.Name)
		}
	}

	var orphaned []string
	for name := range f.schemas {
		if owns(HostServerName, name) {
			continue
		}
		covered := false
		for _, server := range declared {
			if owns(server, name) {
				covered = true
				break
			}
		}
		if !covered {
			orphaned = append(orphaned, name)
		}
	}
	sort.Strings(orphaned)
	if len(orphaned) > 0 {
		return errorf(CodeToolsetMismatch, "the session's tool array carries %s, and this create declares no mcp_server that would serve them; a resume re-declares every server the session froze, with the url and headers that are good now", strings.Join(orphaned, ", "))
	}

	var extra []string
	for _, server := range declared {
		serves := false
		for name := range f.schemas {
			if owns(server, name) {
				serves = true
				break
			}
		}
		if !serves {
			extra = append(extra, server)
		}
	}
	sort.Strings(extra)
	if len(extra) > 0 {
		return errorf(CodeToolsetMismatch, "mcp_server %s serves none of the tools this session froze, and the run sends the array it started with, so nothing it advertises could be reached; start a new session to add a server", strings.Join(extra, ", "))
	}
	return nil
}

// checkFrozenToolset compares what this create resolved against what the
// session froze, and refuses any difference. The array the run sends is the
// frozen one, so a tool that has appeared would never be offered and a tool
// that has gone would be offered with nothing behind it — both of which are
// worse told to the model than to the parent.
//
// The read-only allowance is checked here too, against the copy on the
// session row. session.Resume resolves that map fresh, which on the harness's
// own surface is what lets an operator revoke a server's allowance and have
// the next resume honour it — but here the allowance is supplied by the
// client, in the same create body, so resolving it fresh would let a resume
// re-declare a tool as read_only and be granted something the session never
// had. Refusing any difference is the whole of the rule: a session's
// authorisation is what it started with.
func checkFrozenToolset(ctx context.Context, host *hostTools, f frozenTools, frozenReadOnly map[string]bool) *rpcError {
	defs, readOnly, err := host.Definitions(ctx)
	if err != nil {
		return errorf(CodeToolsetMismatch, "resolve this create's tools: %v", err)
	}
	got := map[string]string{}
	for _, d := range defs {
		if strings.HasPrefix(d.Function.Name, tools.MCPToolPrefix) {
			got[d.Function.Name] = canonicalSchema(d.Function.Parameters)
		}
	}

	var missing, added, changed []string
	for name, want := range f.schemas {
		have, ok := got[name]
		switch {
		case !ok:
			missing = append(missing, name)
		case have != want:
			changed = append(changed, name)
		}
	}
	for name := range got {
		if _, ok := f.schemas[name]; !ok {
			added = append(added, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(added)
	sort.Strings(changed)

	var parts []string
	if len(missing) > 0 {
		parts = append(parts, fmt.Sprintf("%s is in the session's tool array and this create resolved nothing for it", strings.Join(missing, ", ")))
	}
	if len(changed) > 0 {
		parts = append(parts, fmt.Sprintf("%s resolved with a different parameter schema", strings.Join(changed, ", ")))
	}
	if len(added) > 0 {
		parts = append(parts, fmt.Sprintf("%s is new and the session's frozen array has no room for it", strings.Join(added, ", ")))
	}
	if len(parts) > 0 {
		return errorf(CodeToolsetMismatch, "the tools this create resolves are not the ones the session froze: %s. A session sends the array it started with for its whole life; start a new session to change it", strings.Join(parts, "; "))
	}
	// Checked last, so a create that has lost a tool altogether is told
	// that rather than told about the permission the missing tool carried.
	return checkReadOnly(frozenReadOnly, readOnly)
}

// canonicalSchema puts a parameter schema in a form two encodings of the
// same document compare equal in: Go sorts object keys when it marshals a
// map, so a round trip through `any` normalises key order and whitespace
// alike. A schema that will not decode is compared by its own bytes, which
// is the strictest thing left to do with it.
func canonicalSchema(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return string(raw)
	}
	return string(b)
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// checkReadOnly refuses a resume whose read-only allowance differs from the
// one the session started with, in either direction. Widening is the unsafe
// one — a client marking a tool harness.read_only on the way back in would
// be granting itself a call a readonly session was never allowed — and
// narrowing is refused too, because a session that silently lost access to
// half its tools mid-conversation is a worse thing to debug than a create
// that said so.
func checkReadOnly(want, got map[string]bool) *rpcError {
	var widened, narrowed []string
	for name, allowed := range got {
		switch {
		case allowed && !want[name]:
			widened = append(widened, name)
		case !allowed && want[name]:
			narrowed = append(narrowed, name)
		}
	}
	for name, allowed := range want {
		if _, present := got[name]; !present && allowed {
			narrowed = append(narrowed, name)
		}
	}
	sort.Strings(widened)
	sort.Strings(narrowed)

	var parts []string
	if len(widened) > 0 {
		parts = append(parts, fmt.Sprintf("%s is declared read-only now and was not when the session started", strings.Join(widened, ", ")))
	}
	if len(narrowed) > 0 {
		parts = append(parts, fmt.Sprintf("%s was read-only when the session started and is not declared so now", strings.Join(narrowed, ", ")))
	}
	if len(parts) == 0 {
		return nil
	}
	return errorf(CodeToolsetMismatch, "this create changes what a readonly session may call: %s. A session keeps the permissions it was started with; start a new session to change them", strings.Join(parts, "; "))
}
