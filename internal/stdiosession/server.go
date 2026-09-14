package stdiosession

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/hub"
	"github.com/mrgeoffrich/agent-harness/internal/mcpclient"
	"github.com/mrgeoffrich/agent-harness/internal/session"
	"github.com/mrgeoffrich/agent-harness/internal/store"
	"github.com/mrgeoffrich/agent-harness/internal/tools"
	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// Options is everything the server needs, built in cmd/harness the way every
// other composition in this repo is (internal/CLAUDE.md).
type Options struct {
	Store  *store.Store
	Runner *session.Runner
	Hub    *hub.Hub
	// MCP is the manager the interaction's `mcp_server` tools are dialled
	// through. Nil refuses such a tool rather than ignoring it.
	MCP *mcpclient.Manager

	// ServerName is what the handshake reports as server_info.name. It is
	// the subcommand the process was spawned as, prefixed with the binary —
	// "agent-harness stdio-session", or "agent-harness gemini-session" for
	// the alias — because a parent may pin it, and an alias that reported
	// the new name would break the client it exists to keep working. Empty
	// falls back to the current name.
	ServerName string

	// Models is what a create body's `model` may name, and DefaultModel is
	// what it gets when it names none.
	Models       []string
	DefaultModel string

	// HasAPIKey reports whether the API key the named model needs reached
	// this process. It is checked at create so a missing key is one clear
	// error before any work happens, rather than a stream that fails on its
	// first request. It takes the model because this process can host two
	// providers' models from one pipe: a host that supplied a DeepSeek key
	// and no Google one can run the DeepSeek model, and must be told which
	// variable is missing when it asks for the other (missingKeyMessage).
	HasAPIKey func(model string) bool

	Version string

	// Dialect is the parent-facing vocabulary this process speaks. The
	// composition point picks it from the subcommand the process was
	// spawned as; nil is the Responses one, which is what `stdio-session`
	// gets (cmd/harness/stdiosession.go).
	Dialect Dialect
}

// Server implements the protocol over one pipe. One process hosts one
// session: interactions run one at a time, chained by
// previous_interaction_id, which is what lets the shared session.Runner be
// pointed at each interaction's own tool provider in turn.
type Server struct {
	opts Options
	conn *Conn
	// d is opts.Dialect, defaulted. Every method name, wire shape and
	// error string this server produces comes through it.
	d Dialect
	// m is d.Messages(), read once: it is a value and never changes.
	m Messages

	mu           sync.Mutex
	initialized  bool
	handshakeAck bool
	clientCaps   ClientCapabilities
	clientInfo   ClientInfo
	runs         map[string]*run
	order        []string
	running      *run
	shuttingDown bool
	// headers holds the credentials a client's HTTP `mcp_server` tools
	// carry, keyed by server name. They stay here and never reach the
	// database: a bearer token for a loopback server the parent stood up
	// has no business outliving this process, and a -state-dir the parent
	// keeps for resuming would otherwise be a file of plaintext
	// credentials. The row is written without them and
	// mcpclient.Manager.Secrets puts them back for the length of one dial.
	headers map[string]map[string]string
	// env is the same thing for a stdio `mcp_server` tool's env map: a
	// value the client supplied fresh on this create is a connection
	// secret exactly as a bearer header is (docs/STDIO-PROTOCOL.md,
	// "Credentials in headers are never written to disk"), so it is kept
	// here rather than in the mcp_servers.env column internal/store
	// persists for harness serve's own operator-configured servers.
	env map[string]map[string]string
}

// run is one whole agentic run, in the neutral terms this server keeps it.
// It holds no shape either surface puts on the wire: the assembled output
// and the running usage live in the Translator, and the resource a client
// reads is built from these facts and that document together (RunView).
type run struct {
	id        string
	sessionID string
	model     string
	prev      string
	cwd       string

	mu     sync.Mutex
	status string
	// reason, text, result and subTurns are what the loop finished with
	// (internal/session, RunResult), empty until it has.
	reason   string
	text     string
	result   json.RawMessage
	subTurns int
	err      *RunError
	created  time.Time
	updated  time.Time

	// unapplied is the client's message ids for the steers withdrawn when
	// the run ended, in commit order (withdrawSteers).
	unapplied []string

	// inputMu makes an append's status check and its commit one step with
	// respect to the run ending. append holds it from reading the status to
	// recording the message id; record holds it from before the status
	// leaves in_progress until the withdrawal sweep's ids are on the run. So
	// every steer an append committed is either applied by the loop or seen
	// by the sweep, and an append that arrives later is refused. The pump
	// takes it before translating a steer_applied, so an id the append is
	// still recording is on the translator before the echo is built.
	inputMu sync.Mutex
	// messageIDs maps a steer_message seq to the id the client appended it
	// under. Guarded by inputMu.
	messageIDs map[int64]string

	cancel context.CancelFunc
	done   chan struct{}
	tr     Translator
}

// view is the run's facts as a Translator takes them. It is read under the
// lock, so a get racing the run's end sees one consistent set.
func (it *run) view(withItems bool) RunView {
	it.mu.Lock()
	defer it.mu.Unlock()
	return RunView{
		ID: it.id, Model: it.model, Status: it.status, SessionID: it.sessionID,
		Created: it.created, Updated: it.updated,
		Reason: it.reason, Text: it.text, Result: it.result,
		SubTurns: it.subTurns, Err: it.err, WithItems: withItems,
		UnappliedMessageIDs: append([]string(nil), it.unapplied...),
	}
}

// NewServer wires a server onto r/w. Serve runs it.
func NewServer(opts Options) *Server {
	d := opts.Dialect
	if d == nil {
		d = NewResponses()
	}
	s := &Server{
		opts: opts, d: d, m: d.Messages(), runs: map[string]*run{},
		headers: map[string]map[string]string{}, env: map[string]map[string]string{},
	}
	if opts.MCP != nil {
		opts.MCP.Secrets = s.dialSecrets
	}
	return s
}

// dialSecrets puts a declared server's headers or stdio env back on the row
// for the length of one dial. A server this process never saw declared is
// returned unchanged, which means it is dialled with whatever the row holds
// on its own — nothing, for either kind of secret.
func (s *Server) dialSecrets(srv store.MCPServer) store.MCPServer {
	s.mu.Lock()
	h := s.headers[srv.Name]
	e := s.env[srv.Name]
	s.mu.Unlock()
	if len(h) > 0 {
		srv.Headers = h
	}
	if len(e) > 0 {
		srv.Env = e
	}
	return srv
}

// Serve reads the pipe until it ends. It returns when stdin closes, which is
// the parent saying the session is over: a run still in flight is cancelled
// and given the chance to record its own terminal event before this returns.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	s.conn = NewConn(in, out, s.handle, isHandshake)
	err := s.conn.Serve(ctx)
	s.stopRunning("the client closed the connection")
	return err
}

// stopRunning cancels whatever is in flight and waits for it to record its
// terminal event.
func (s *Server) stopRunning(reason string) {
	s.mu.Lock()
	it := s.running
	s.shuttingDown = true
	s.mu.Unlock()
	if it == nil {
		return
	}
	it.mu.Lock()
	cancel := it.cancel
	it.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	<-it.done
}

func (s *Server) handle(ctx context.Context, method string, params json.RawMessage) (any, *rpcError) {
	if method == MethodInitialize {
		return s.initialize(params)
	}
	if method == MethodInitialized {
		s.mu.Lock()
		s.handshakeAck = s.initialized
		s.mu.Unlock()
		return nil, nil
	}

	s.mu.Lock()
	ready := s.initialized && s.handshakeAck
	s.mu.Unlock()
	if !ready {
		if isNotification(method) {
			// An unrecognised notification is ignored, never an error —
			// including one that arrives too early.
			return nil, nil
		}
		return nil, errorf(CodeNotInitialized, "%s before the initialize/initialized handshake", method)
	}

	switch verbs := s.d.Methods(); method {
	case verbs.Create:
		return s.create(ctx, params)
	case verbs.Append:
		return s.append(ctx, params)
	case verbs.Cancel:
		return s.cancelRun(params)
	case verbs.Get:
		return s.get(params)
	case verbs.Delete:
		return s.delete(params)
	case MethodShutdown:
		go s.stopRunning("the client asked this session to shut down")
		return map[string]any{}, nil
	default:
		if isNotification(method) {
			return nil, nil
		}
		return nil, errorf(CodeMethodNotFound, "no method %q; see docs/STDIO-PROTOCOL.md", method)
	}
}

// isHandshake names the two frames that must be handled in arrival order.
// A client sends initialize and initialized back to back without waiting,
// and handling them concurrently would let the acknowledgement be seen
// before the request it acknowledges — after which nothing would ever be
// accepted.
func isHandshake(method string) bool {
	return method == MethodInitialize || method == MethodInitialized
}

// isNotification reports whether method is one this side treats as fire and
// forget. A notification is never answered with an error, so an unrecognised
// one has to be recognisable as a notification by its name; anything the
// client sends with an id gets a proper method-not-found instead.
func isNotification(method string) bool {
	return method == MethodInitialized || strings.HasSuffix(method, "/notify") ||
		strings.HasPrefix(method, "notifications/")
}

func (s *Server) initialize(params json.RawMessage) (any, *rpcError) {
	var p InitializeParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, errorf(CodeInvalidParams, "initialize params: %v", err)
		}
	}
	s.mu.Lock()
	if s.initialized {
		s.mu.Unlock()
		return nil, errorf(CodeInvalidRequest, "initialize was already called")
	}
	s.initialized = true
	s.clientCaps = p.Capabilities
	s.clientInfo = p.ClientInfo
	s.mu.Unlock()

	name := s.opts.ServerName
	if name == "" {
		name = "agent-harness stdio-session"
	}
	return s.d.Initialize(Handshake{
		Name:            name,
		Version:         s.opts.Version,
		Models:          s.opts.Models,
		DefaultModel:    s.opts.DefaultModel,
		Details:         modelDetailsFor(s.opts.Models),
		MCPServers:      s.opts.MCP != nil,
		PermissionModes: []string{string(tools.ModeReadOnly), string(tools.ModeFull)},
	}), nil
}

// --- create ---

func (s *Server) create(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	p, rerr := s.d.DecodeCreate(params)
	if rerr != nil {
		return nil, rerr
	}
	if p.Harness != nil && len(p.Harness.MaxSubTurns) > 0 {
		return nil, errorf(CodeUnsupported, "harness.max_sub_turns: this process does not cap a run by sub-turns; a run ends when the model finishes or the client cancels it")
	}
	model := p.Model
	if model == "" {
		model = s.opts.DefaultModel
	}
	// The handshake's own list, not provider.Known: this binary hosts a
	// chosen few of the models the repository can route, and a model it can
	// route but did not advertise — DeepSeek's two text-only models, which
	// would arrive carrying vision tools that need a second provider's
	// credentials to work (docs/DEEPSEEK-VISION.md) — must be refused here
	// rather than started.
	if !accepts(s.opts.Models, model) {
		return nil, errorf(CodeInvalidParams, "unknown model %q; this process accepts %s", model, strings.Join(s.opts.Models, ", "))
	}
	// A level the model refuses is caught here rather than by Google. Left
	// to the API it is a 400 in the middle of a started run: the create is
	// answered, steps stream, and the interaction ends `failed` carrying a
	// message about a request the client cannot see. The levels are on the
	// handshake, so a client has been told which are allowed.
	if effort := p.Effort; effort != "" && !reasoningEffortSupported(model, effort) {
		return nil, errorf(CodeInvalidParams, s.m.EffortRefused, effort, model, strings.Join(reasoningEfforts(model), ", "))
	}
	if s.opts.HasAPIKey != nil && !s.opts.HasAPIKey(model) {
		return nil, errorf(CodeCredentialsMissing, "%s", missingKeyMessage(model))
	}

	text := p.Prompt
	if p.Instructions != "" {
		// Prepended rather than replacing the harness's own system prompt,
		// which is frozen for a session's life and is the shared prefix the
		// prompt cache is built on (docs/CACHE.md). A parent's mode
		// fragment or session preamble belongs on the first user message,
		// which is where this puts it.
		text = p.Instructions + "\n\n" + text
	}

	s.mu.Lock()
	if s.shuttingDown {
		s.mu.Unlock()
		return nil, errorf(CodeInvalidRequest, "this session is shutting down")
	}
	if s.running != nil {
		id := s.running.id
		s.mu.Unlock()
		return nil, errorf(CodeInvalidRequest, s.m.AlreadyRunning, id)
	}
	s.mu.Unlock()

	// Three ways to reach a session. previous_interaction_id continues one
	// this process is already holding — Google's own multi-turn shape, and
	// the reason there is no thread concept here. harness.resume_session_id
	// picks one up out of the state directory instead, which is the only
	// one of the two that survives this process ending (resume.go). Neither
	// names a session: a create with no id at all starts one.
	var (
		sessionID string
		cwd       string
		resume    bool
		// frozen is the session's stored tool array, and frozenMode its
		// stored permission mode, on a resume; both are what the run will
		// use regardless of what the create says, so both are checked
		// against the create rather than taken from it.
		frozen         frozenTools
		frozenReadOnly map[string]bool
		checkTools     bool
		frozenMode     string
	)
	resumeID := harnessString(p.Harness, func(h *CreateHarness) string { return h.ResumeSessionID })
	switch {
	case p.PreviousRunID != "" && resumeID != "":
		return nil, errorf(CodeInvalidParams, "%s", s.m.BothContinuations)

	case p.PreviousRunID != "":
		prev, ok := s.lookup(p.PreviousRunID)
		if !ok {
			return nil, errorf(CodeRunNotFound, s.m.PreviousNotFound, p.PreviousRunID)
		}
		sessionID, cwd, resume = prev.sessionID, prev.cwd, true
		if p.Harness != nil && p.Harness.CWD != "" && p.Harness.CWD != cwd {
			return nil, errorf(CodeInvalidParams, s.m.PreviousChangedCWD, prev.id, cwd)
		}
		if prev.model != model {
			return nil, errorf(CodeInvalidParams, s.m.PreviousChangedModel, prev.id, prev.model)
		}

	case resumeID != "":
		sess, rerr := s.resumeTarget(ctx, p)
		if rerr != nil {
			return nil, rerr
		}
		sessionID, cwd, resume, model = sess.ID, sess.Workspace, true, sess.Model
		frozenMode = sess.PermissionMode
		f, err := frozenToolsOf(sess.ToolSchema, sess.MCPReadOnly)
		if err != nil {
			return nil, errorf(CodeInternalError, "session %s's stored tool array will not decode (%v), so this create cannot be checked against it", sess.ID, err)
		}
		if rerr := checkDeclarations(f, p.Tools); rerr != nil {
			return nil, rerr
		}
		frozen, frozenReadOnly, checkTools = f, sess.MCPReadOnly, true

	default:
		if p.Harness == nil || p.Harness.CWD == "" {
			return nil, errorf(CodeInvalidParams, "harness.cwd is required: this process works in a directory the client owns and does not choose one for itself")
		}
		sessionID = session.NewSessionID()
		cwd = p.Harness.CWD
	}

	mode := tools.Mode(harnessString(p.Harness, func(h *CreateHarness) string { return h.PermissionMode }))
	if mode == "" {
		mode = tools.ModeReadOnly
	}
	if !mode.Valid() {
		return nil, errorf(CodeInvalidParams, "harness.permission_mode %q must be %q or %q", mode, tools.ModeReadOnly, tools.ModeFull)
	}
	if frozenMode != "" {
		// Resume takes the mode off the session row, so this is what the
		// run will actually enforce; resumeTarget has already refused a
		// create that named a different one.
		mode = tools.Mode(frozenMode)
	}

	host, rerr := s.buildTools(ctx, p.Tools)
	if rerr != nil {
		return nil, rerr
	}
	if checkTools {
		if rerr := checkFrozenToolset(ctx, host, frozen, frozenReadOnly); rerr != nil {
			return nil, rerr
		}
	}

	it := &run{
		id: s.d.NewRunID(), sessionID: sessionID, model: model,
		prev: p.PreviousRunID, cwd: cwd,
		status: StatusInProgress, created: time.Now().UTC(), updated: time.Now().UTC(),
		done: make(chan struct{}), messageIDs: map[int64]string{},
	}
	host.runID = it.id

	s.mu.Lock()
	s.runs[it.id] = it
	s.order = append(s.order, it.id)
	s.running = it
	// The runner is shared and its MCP field is per interaction. Only one
	// interaction runs at a time (the guard above), and this is set before
	// the run starts and left alone until it ends, so the field is never
	// written while a loop is reading it.
	s.opts.Runner.MCP = host
	s.mu.Unlock()

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	it.mu.Lock()
	it.cancel = cancel
	it.mu.Unlock()

	frames, unsubscribe := s.opts.Hub.Subscribe(sessionID)
	tr := s.d.NewTranslator(it.id, model, s.notify)
	if p.Harness != nil {
		tr.SetFirstMessageID(p.Harness.MessageID)
	}
	it.mu.Lock()
	it.tr = tr
	it.mu.Unlock()
	tr.Created()

	pu := newPump()
	go pu.drain(frames)
	streamed := make(chan struct{})
	go func() {
		defer close(streamed)
		for {
			f, ok := pu.next()
			if !ok {
				return
			}
			switch {
			case f.Live != nil:
				tr.Live(*f.Live)
			case f.State != nil:
				// The session's own metadata row. Nothing on it is part of
				// the interaction: status and usage both reach the client
				// through the events themselves.
			default:
				// An append may still be recording this steer's message id
				// (run.inputMu).
				if f.Event.Kind == store.KindSteerApplied {
					it.inputMu.Lock()
					it.inputMu.Unlock()
				}
				tr.Event(f.Event)
			}
		}
	}()

	opts := session.RunOptions{
		Model:          model,
		Effort:         effortFrom(p.Effort),
		Thinking:       true,
		MaxTokens:      p.MaxOutputTokens,
		Workspace:      cwd,
		PermissionMode: mode,
		Deny:           harnessSlice(p.Harness, func(h *CreateHarness) []string { return h.Deny }),
		Prompt:         text,
		SessionID:      sessionID,
		// Deliberately still the old command name. This is a stored column
		// on the session row, not a name anybody types: changing it would
		// split one label across every store written before and after the
		// rename, for nothing a reader gains.
		JobType:      "gemini-session",
		ParentIsUser: true,
		Title:        harnessString(p.Harness, func(h *CreateHarness) string { return h.Title }),
		Description:  harnessString(p.Harness, func(h *CreateHarness) string { return h.Description }),
	}
	if schema := p.ResultSchema; len(schema) > 0 {
		opts.ResultSchema = schema
	}

	go func() {
		defer close(it.done)
		var (
			res *session.RunResult
			err error
		)
		if resume {
			res, err = s.opts.Runner.Resume(runCtx, session.ResumeOptions{
				SessionID: sessionID, Prompt: text,
				MaxTokens: opts.MaxTokens,
			})
		} else {
			res, err = s.opts.Runner.Run(runCtx, opts)
		}
		// The run has recorded its terminal event by now, so unsubscribing
		// here cannot lose it: a closed channel still yields what is already
		// buffered in it, and the pump drains to exhaustion before closing.
		unsubscribe()
		<-streamed
		s.record(it, tr, res, err, runCtx.Err() != nil)
		cancel()

		s.mu.Lock()
		if s.running == it {
			s.running = nil
			s.opts.Runner.MCP = nil
		}
		s.mu.Unlock()

		// Emitted after the slot is free, never before. A client that
		// reacts to interaction.completed by creating the next interaction
		// is doing the obvious thing, and it used to race the bookkeeping
		// and be told the interaction was still running.
		s.complete(it, tr)
	}()

	if p.Stream != nil && !*p.Stream {
		// Google's non-streaming create answers with the whole finished
		// interaction. The step notifications are sent either way; a client
		// that asked for stream:false and ignores them gets exactly Google's
		// behaviour.
		select {
		case <-it.done:
		case <-ctx.Done():
			return nil, errorf(CodeInternalError, s.m.ContextEnded, it.id, ctx.Err())
		}
		return it.tr.Result(it.view(true)), nil
	}
	return it.tr.Result(it.view(false)), nil
}

// record writes the run's outcome onto the interaction and, for a run the
// loop could not finish, emits the `error` notification that precedes the
// terminal frame. complete emits the terminal frame itself, once the
// interaction is no longer the running one.
//
// The run leaves in_progress here, with inputMu held across the withdrawal
// sweep, so no append commits a steer after the sweep has looked.
func (s *Server) record(it *run, tr Translator, res *session.RunResult, err error, cancelled bool) {
	tr.CloseText()

	it.inputMu.Lock()
	defer it.inputMu.Unlock()
	unapplied := s.withdrawSteers(it)

	it.mu.Lock()
	it.unapplied = unapplied
	it.updated = time.Now().UTC()
	switch {
	case cancelled:
		it.status = StatusCancelled
		it.reason = "cancelled"
	case err != nil:
		it.status = StatusFailed
		it.err = &RunError{Code: "internal", Message: err.Error()}
	case res != nil:
		it.status = statusFor(res)
		it.reason = res.Reason
		it.text = res.Text
		it.result = res.Result
		it.subTurns = res.SubTurns
	default:
		it.status = StatusFailed
	}
	it.mu.Unlock()

	if err != nil && !cancelled {
		tr.Failed(it.view(false))
	}
}

// withdrawSteers closes every steer committed to the run's session that the
// loop never applied, so no later run delivers it, and returns the client's
// message ids for them in commit order. A steer appended without an id is
// withdrawn and contributes none. The caller holds it.inputMu.
//
// The run's own context is usually cancelled by now, and the store refuses
// work on a cancelled context, so the sweep gets its own.
func (s *Server) withdrawSteers(it *run) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	seqs, err := s.opts.Store.WithdrawUnappliedSteers(ctx, it.sessionID)
	if err != nil {
		log.Printf("stdiosession: withdraw unapplied steers for %s: %v", it.sessionID, err)
		return nil
	}
	var ids []string
	for _, seq := range seqs {
		if id := it.messageIDs[seq]; id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// complete emits the last notification a run ever produces.
func (s *Server) complete(it *run, tr Translator) {
	tr.Completed(it.view(true))
}

// statusFor maps a run's terminal reason onto the surface's status enum: a
// failed run is `failed`, and everything else that ended on its own terms is
// `completed`, with harness.reason carrying which way (internal/session,
// RunResult.Reason).
func statusFor(res *session.RunResult) string {
	if res.Status == store.StatusFailed {
		return StatusFailed
	}
	return StatusCompleted
}

// --- append, cancel, get, delete ---

func (s *Server) append(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	p, rerr := s.d.DecodeAppend(params)
	if rerr != nil {
		return nil, rerr
	}
	it, ok := s.lookup(p.RunID)
	if !ok {
		return nil, errorf(CodeRunNotFound, s.m.NotFound, p.RunID)
	}
	it.inputMu.Lock()
	defer it.inputMu.Unlock()
	it.mu.Lock()
	status, tr := it.status, it.tr
	it.mu.Unlock()
	if status != StatusInProgress {
		return nil, errorf(CodeRunNotRunning, s.m.AppendNotRunning, it.id, status)
	}
	if p.Prompt == "" {
		return nil, errorf(CodeInvalidParams, "%s", s.m.AppendNeedsInput)
	}

	appended, err := s.opts.Store.AppendEvents(ctx, it.sessionID, []store.EventInput{{
		Kind:    store.KindSteerMessage,
		Payload: store.SteerMessagePayload{Text: p.Prompt, Source: "cli"},
	}})
	if errors.Is(err, store.ErrSessionCancelled) {
		// The loop has marked the session cancelled and record has not yet
		// moved the run out of in_progress. A moment later this append would
		// be refused on the status, so it is refused the same way now.
		return nil, errorf(CodeRunNotRunning, s.m.AppendNotRunning, it.id, StatusCancelled)
	}
	if err != nil {
		return nil, errorf(CodeInternalError, "record the input: %v", err)
	}
	seq := appended[0].Seq
	if p.MessageID != "" {
		it.messageIDs[seq] = p.MessageID
		if tr != nil {
			tr.SetMessageID(seq, p.MessageID)
		}
	}
	return s.d.AppendResult(it.id, seq), nil
}

func (s *Server) cancelRun(params json.RawMessage) (any, *rpcError) {
	id, rerr := s.d.DecodeID(s.d.Methods().Cancel, params)
	if rerr != nil {
		return nil, rerr
	}
	it, ok := s.lookup(id)
	if !ok {
		return nil, errorf(CodeRunNotFound, s.m.NotFound, id)
	}
	it.mu.Lock()
	cancel, status := it.cancel, it.status
	it.mu.Unlock()
	if status != StatusInProgress {
		// Cancelling something already finished is not an error: it is the
		// state the caller asked for, and a client racing a completion
		// should not have to handle both outcomes.
		return it.tr.Result(it.view(true)), nil
	}
	if cancel != nil {
		cancel()
	}
	<-it.done
	return it.tr.Result(it.view(true)), nil
}

func (s *Server) get(params json.RawMessage) (any, *rpcError) {
	id, rerr := s.d.DecodeID(s.d.Methods().Get, params)
	if rerr != nil {
		return nil, rerr
	}
	it, ok := s.lookup(id)
	if !ok {
		return nil, errorf(CodeRunNotFound, s.m.NotFound, id)
	}
	return it.tr.Result(it.view(true)), nil
}

func (s *Server) delete(params json.RawMessage) (any, *rpcError) {
	id, rerr := s.d.DecodeID(s.d.Methods().Delete, params)
	if rerr != nil {
		return nil, rerr
	}
	s.mu.Lock()
	it, ok := s.runs[id]
	if ok && s.running == it {
		s.mu.Unlock()
		return nil, errorf(CodeRunNotRunning, s.m.DeleteStillRunning, id)
	}
	delete(s.runs, id)
	for i, have := range s.order {
		if have == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	s.mu.Unlock()
	if !ok {
		return nil, errorf(CodeRunNotFound, s.m.NotFound, id)
	}
	// Google's delete answers with an empty body. The session's own
	// transcript on disk is untouched: this forgets the run, it does
	// not erase the run.
	return map[string]any{}, nil
}

// --- helpers ---

func (s *Server) lookup(id string) (*run, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.runs[id]
	return it, ok
}

// notify writes one notification. A write failure means the pipe has gone,
// which the read side is already discovering; there is nowhere to report it.
func (s *Server) notify(method string, params any) {
	if s.conn == nil {
		return
	}
	_ = s.conn.Notify(method, params)
}

// buildTools turns the create body's `tools` array into the provider the run
// reaches its non-built-in tools through.
func (s *Server) buildTools(ctx context.Context, decls []Tool) (*hostTools, *rpcError) {
	host := newHostTools(nil, s.d, "", nil)
	var servers []Tool
	for _, t := range decls {
		switch t.Type {
		case ToolFunction:
			if t.Name == HostServerName {
				return nil, errorf(CodeInvalidParams, "%q is reserved as the namespace client function tools are offered under", HostServerName)
			}
			if err := host.addFunction(t); err != nil {
				return nil, errorf(CodeInvalidParams, "function tool: %v", err)
			}
		case ToolMCPServer:
			servers = append(servers, t)
		case "":
			return nil, errorf(CodeInvalidParams, "every tool needs a type")
		default:
			return nil, errorf(CodeUnsupported, "tool type %q is one Google runs on its own machines; this process runs its tools here, so only %q and %q are accepted", t.Type, ToolFunction, ToolMCPServer)
		}
	}

	if host.hasFunctions() {
		s.mu.Lock()
		caps := s.clientCaps
		s.mu.Unlock()
		if !caps.FunctionCalls {
			return nil, errorf(CodeInvalidParams, "the create body declares function tools but the client did not claim the function_calls capability at initialize, so there would be nothing to call them with")
		}
		host.call = s.callFunction
	}

	if len(servers) > 0 {
		if s.opts.MCP == nil {
			return nil, errorf(CodeUnsupported, "this process was started with no MCP client, so an mcp_server tool cannot be dialled")
		}
		for _, t := range servers {
			host.allow[t.Name] = true
		}
		if err := s.registerServers(ctx, servers, host.names); err != nil {
			return nil, errorf(CodeInvalidParams, "mcp_server tool: %v", err)
		}
		host.mcp = s.opts.MCP
	}
	return host, nil
}

// registerServers records each declared server and probes it, which is what
// puts its tool list in the snapshot the loop's frozen tool array is built
// from (docs/MCP.md, "Resolution happens once per run"). A server that fails
// to probe contributes no tools and does not fail the create: the same
// tolerance an operator-configured server gets.
//
// A declaration dials over HTTP (url, optionally headers) or over stdio
// (command, optionally args and env) — internal/mcpclient's dialer already
// speaks both (dial.go), so this is only the wire-level translation into the
// store.MCPServer row the dialer reads. Naming both pairs, or neither, is
// refused before anything is written or dialled.
func (s *Server) registerServers(ctx context.Context, servers []Tool, names map[string]bool) error {
	for _, t := range servers {
		if t.Name == "" {
			return fmt.Errorf("an mcp_server tool needs a name")
		}
		if t.Name == HostServerName {
			return fmt.Errorf("%q is reserved", HostServerName)
		}
		hasHTTP := t.URL != "" || len(t.Headers) > 0
		hasStdio := t.Command != "" || len(t.Args) > 0 || len(t.Env) > 0
		if hasHTTP && hasStdio {
			return fmt.Errorf("mcp_server %q names both url/headers and command/args/env; a server dials over exactly one transport", t.Name)
		}
		if !hasHTTP && !hasStdio {
			return fmt.Errorf("mcp_server %q needs a url (to dial it over http) or a command (to dial it over stdio)", t.Name)
		}

		var row store.MCPServer
		switch {
		case hasHTTP:
			if t.URL == "" {
				return fmt.Errorf("mcp_server %q sets headers but no url", t.Name)
			}
			row = store.MCPServer{
				Name: t.Name, Transport: store.MCPTransportHTTP,
				URL: t.URL, Enabled: true,
				AllowReadOnly: t.Harness != nil && t.Harness.ReadOnly,
			}
		default:
			if t.Command == "" {
				return fmt.Errorf("mcp_server %q sets args or env but no command", t.Name)
			}
			row = store.MCPServer{
				Name: t.Name, Transport: store.MCPTransportStdio,
				Command: t.Command, Args: t.Args, Enabled: true,
				AllowReadOnly: t.Harness != nil && t.Harness.ReadOnly,
			}
		}
		// Headers and stdio env are deliberately absent from the row: they
		// are the one part of a declaration that is a credential, and this
		// process keeps them in memory (Server.headers, Server.env) so a
		// state directory the parent keeps holds neither.
		s.mu.Lock()
		if len(t.Headers) > 0 {
			s.headers[t.Name] = t.Headers
		} else {
			delete(s.headers, t.Name)
		}
		if len(t.Env) > 0 {
			s.env[t.Name] = t.Env
		} else {
			delete(s.env, t.Name)
		}
		s.mu.Unlock()
		if _, err := s.opts.Store.GetMCPServer(ctx, t.Name); err == nil {
			if err := s.opts.Store.UpdateMCPServer(ctx, row); err != nil {
				return err
			}
		} else if errors.Is(err, store.ErrMCPServerNotFound) {
			if err := s.opts.Store.CreateMCPServer(ctx, row); err != nil {
				return err
			}
		} else {
			return err
		}
		row, err := s.opts.MCP.Refresh(ctx, t.Name)
		if err != nil {
			// Recorded on the row by Refresh itself; the run continues with
			// whatever the server did advertise, which is none of it.
			continue
		}
		// The probe is the only place a server's own tool names are known
		// exactly, so this is where the interaction records which qualified
		// names are its to offer and to call.
		for _, tool := range row.Tools {
			names[tool.QualifiedName] = true
		}
	}
	return nil
}

// callFunction asks the client to run one function tool and waits.
func (s *Server) callFunction(ctx context.Context, params any) (json.RawMessage, error) {
	var res json.RawMessage
	if err := s.conn.Call(ctx, MethodFunctionCall, params, &res); err != nil {
		return nil, err
	}
	return res, nil
}

// effortFrom maps the create body's reasoning.effort onto the loop's effort.
// They are the same vocabulary — the loop's wire.Effort* values are what
// DeepSeek's reasoning_effort takes — so this passes through, and a provider
// whose spellings differ maps it in its own client (internal/gemini's
// intent.go does).
func effortFrom(effort string) string {
	if effort == "" {
		return wire.EffortHigh
	}
	return effort
}

func harnessString(h *CreateHarness, get func(*CreateHarness) string) string {
	if h == nil {
		return ""
	}
	return get(h)
}

func harnessSlice(h *CreateHarness, get func(*CreateHarness) []string) []string {
	if h == nil {
		return nil
	}
	return get(h)
}
