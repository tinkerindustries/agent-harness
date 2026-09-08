package geministdio

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/gemini"
	"github.com/mrgeoffrich/agent-harness/internal/hub"
	"github.com/mrgeoffrich/agent-harness/internal/mcpclient"
	"github.com/mrgeoffrich/agent-harness/internal/provider"
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

	// Models is what a create body's `model` may name, and DefaultModel is
	// what it gets when it names none.
	Models       []string
	DefaultModel string

	// HasAPIKey reports whether a Google API key reached this process. It is
	// checked at create so a missing key is one clear error before any work
	// happens, rather than a stream that fails on its first request.
	HasAPIKey func() bool

	Version string
}

// Server implements the protocol over one pipe. One process hosts one
// session: interactions run one at a time, chained by
// previous_interaction_id, which is what lets the shared session.Runner be
// pointed at each interaction's own tool provider in turn.
type Server struct {
	opts Options
	conn *Conn

	mu           sync.Mutex
	initialized  bool
	handshakeAck bool
	clientCaps   ClientCapabilities
	clientInfo   ClientInfo
	interactions map[string]*interaction
	order        []string
	running      *interaction
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

// interaction is one run, in the shape Google's Interaction resource
// describes it.
type interaction struct {
	id        string
	sessionID string
	model     string
	prev      string
	cwd       string

	mu      sync.Mutex
	status  string
	steps   []Step
	usage   Usage
	errors  []Error
	harness InteractionHarness
	created time.Time
	updated time.Time

	cancel context.CancelFunc
	done   chan struct{}
	tr     *translator
}

// newInteractionID mints the id an interaction is addressed by. It is not
// the session id: a chain of interactions linked by previous_interaction_id
// is one session resumed repeatedly, so the two cannot be the same value.
// harness.session_id on every interaction carries the session's own.
func newInteractionID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("geministdio: crypto/rand unavailable: " + err.Error())
	}
	return "int_" + hex.EncodeToString(b[:])
}

// NewServer wires a server onto r/w. Serve runs it.
func NewServer(opts Options) *Server {
	s := &Server{
		opts: opts, interactions: map[string]*interaction{},
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

	switch method {
	case MethodInteractionsCreate:
		return s.create(ctx, params)
	case MethodInteractionsAppend:
		return s.append(ctx, params)
	case MethodInteractionsCancel:
		return s.cancelInteraction(params)
	case MethodInteractionsGet:
		return s.get(params)
	case MethodInteractionsDelete:
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

	return InitializeResult{
		ServerInfo: ServerInfo{
			Name:     "agent-harness gemini-session",
			Version:  s.opts.Version,
			Protocol: "google.interactions.v1beta",
		},
		Capabilities: ServerCapabilities{
			Streaming:           true,
			Append:              true,
			Cancel:              true,
			PreviousInteraction: true,
			ResumeSession:       true,
			MCPServers:          s.opts.MCP != nil,
			FunctionTools:       true,
			PermissionModes:     []string{string(tools.ModeReadOnly), string(tools.ModeFull)},
		},
		Models:       s.opts.Models,
		DefaultModel: s.opts.DefaultModel,
		ModelDetails: modelDetails(s.opts.Models),
	}, nil
}

// modelDetails is the per-model capability array the handshake advertises,
// one entry per model this process accepts, in the same order. A field
// internal/gemini has no table entry for comes back zero/empty on that
// model's entry rather than being guessed at, which is the same thing its
// absence means to a client: nothing here constrains it.
func modelDetails(models []string) []ModelDetail {
	out := make([]ModelDetail, 0, len(models))
	for _, m := range models {
		out = append(out, ModelDetail{
			ID:                  m,
			DisplayName:         gemini.DisplayName(m),
			ContextWindowTokens: gemini.ContextWindowTokens(m),
			ThinkingLevels:      gemini.LevelsFor(m),
		})
	}
	return out
}

// --- create ---

func (s *Server) create(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var p CreateParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, errorf(CodeInvalidParams, "interactions.create params: %v", err)
	}
	if p.Agent != "" {
		return nil, errorf(CodeUnsupported, "agent %q: Google's managed agents run on Google's machines; this process runs the loop on yours, so only `model` is accepted", p.Agent)
	}
	model := p.Model
	if model == "" {
		model = s.opts.DefaultModel
	}
	if !provider.Known(model) {
		return nil, errorf(CodeInvalidParams, "unknown model %q; this process accepts %s", model, strings.Join(s.opts.Models, ", "))
	}
	// A level the model refuses is caught here rather than by Google. Left
	// to the API it is a 400 in the middle of a started run: the create is
	// answered, steps stream, and the interaction ends `failed` carrying a
	// message about a request the client cannot see. The levels are on the
	// handshake, so a client has been told which are allowed.
	if level := thinkingLevelOf(p.GenerationConfig); level != "" && !gemini.LevelSupported(model, level) {
		return nil, errorf(CodeInvalidParams, "generation_config.thinking_level %q: %s accepts %s",
			level, model, strings.Join(gemini.LevelsFor(model), ", "))
	}
	if s.opts.HasAPIKey != nil && !s.opts.HasAPIKey() {
		return nil, errorf(CodeCredentialsMissing, "no Google API key reached this process: set GEMINI_API_KEY (or GOOGLE_API_KEY) in the environment you spawn it with")
	}

	text, rerr := inputText(p.Input)
	if rerr != nil {
		return nil, rerr
	}
	if p.SystemInstruction != "" {
		// Prepended rather than replacing the harness's own system prompt,
		// which is frozen for a session's life and is the shared prefix the
		// prompt cache is built on (docs/CACHE.md). A parent's mode
		// fragment or session preamble belongs on the first user message,
		// which is where this puts it.
		text = p.SystemInstruction + "\n\n" + text
	}

	s.mu.Lock()
	if s.shuttingDown {
		s.mu.Unlock()
		return nil, errorf(CodeInvalidRequest, "this session is shutting down")
	}
	if s.running != nil {
		id := s.running.id
		s.mu.Unlock()
		return nil, errorf(CodeInvalidRequest, "interaction %s is still running; one process hosts one session, so cancel it or wait for it to complete", id)
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
	case p.PreviousInteractionID != "" && resumeID != "":
		return nil, errorf(CodeInvalidParams, "previous_interaction_id and harness.resume_session_id both name a conversation to continue; send one. previous_interaction_id continues an interaction this process ran, harness.resume_session_id continues a session out of the state directory")

	case p.PreviousInteractionID != "":
		prev, ok := s.lookup(p.PreviousInteractionID)
		if !ok {
			return nil, errorf(CodeInteractionNotFound, "no interaction %q was minted by this process; an interaction id does not outlive the process that made it, so continue across a restart with harness.resume_session_id instead", p.PreviousInteractionID)
		}
		sessionID, cwd, resume = prev.sessionID, prev.cwd, true
		if p.Harness != nil && p.Harness.CWD != "" && p.Harness.CWD != cwd {
			return nil, errorf(CodeInvalidParams, "interaction %s works in %s; a continued interaction cannot change directory, and inherits the one its chain started in", prev.id, cwd)
		}
		if prev.model != model {
			return nil, errorf(CodeInvalidParams, "interaction %s ran on %s; a continued interaction cannot change model, because the session's prefix is frozen", prev.id, prev.model)
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

	it := &interaction{
		id: newInteractionID(), sessionID: sessionID, model: model,
		prev: p.PreviousInteractionID, cwd: cwd,
		status: StatusInProgress, created: time.Now().UTC(), updated: time.Now().UTC(),
		done: make(chan struct{}),
	}
	it.harness.SessionID = sessionID
	host.interactionID = it.id

	s.mu.Lock()
	s.interactions[it.id] = it
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
	tr := newTranslator(it.id, model, s.notify)
	if p.Harness != nil {
		tr.setFirstMessageID(p.Harness.MessageID)
	}
	it.mu.Lock()
	it.tr = tr
	it.mu.Unlock()
	tr.created()

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
				tr.live(*f.Live)
			case f.State != nil:
				// The session's own metadata row. Nothing on it is part of
				// the interaction: status and usage both reach the client
				// through the events themselves.
			default:
				tr.event(f.Event)
			}
		}
	}()

	opts := session.RunOptions{
		Model:          model,
		Effort:         effortFrom(p.GenerationConfig),
		Thinking:       true,
		MaxTokens:      maxTokensFrom(p.GenerationConfig),
		Workspace:      cwd,
		PermissionMode: mode,
		Deny:           harnessSlice(p.Harness, func(h *CreateHarness) []string { return h.Deny }),
		Prompt:         text,
		SessionID:      sessionID,
		JobType:        "gemini-session",
		ParentIsUser:   true,
		Title:          harnessString(p.Harness, func(h *CreateHarness) string { return h.Title }),
		Description:    harnessString(p.Harness, func(h *CreateHarness) string { return h.Description }),
		MaxSubTurns:    harnessInt(p.Harness, func(h *CreateHarness) int { return h.MaxSubTurns }),
	}
	if p.ResponseFormat != nil && len(p.ResponseFormat.Schema) > 0 {
		opts.ResultSchema = p.ResponseFormat.Schema
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
				MaxTokens: opts.MaxTokens, MaxSubTurns: opts.MaxSubTurns,
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
		s.complete(it)
	}()

	if p.Stream != nil && !*p.Stream {
		// Google's non-streaming create answers with the whole finished
		// interaction. The step notifications are sent either way; a client
		// that asked for stream:false and ignores them gets exactly Google's
		// behaviour.
		select {
		case <-it.done:
		case <-ctx.Done():
			return nil, errorf(CodeInternalError, "interaction %s: %v", it.id, ctx.Err())
		}
		return CreateResult{Interaction: it.snapshot(true)}, nil
	}
	return CreateResult{Interaction: it.snapshot(false)}, nil
}

// record writes the run's outcome onto the interaction and, for a run the
// loop could not finish, emits the `error` notification that precedes the
// terminal frame. complete emits the terminal frame itself, once the
// interaction is no longer the running one.
func (s *Server) record(it *interaction, tr *translator, res *session.RunResult, err error, cancelled bool) {
	tr.closeText()

	steps, usage := tr.snapshot()
	it.mu.Lock()
	it.steps = steps
	it.usage = usage
	it.tr = nil
	it.updated = time.Now().UTC()
	switch {
	case cancelled:
		it.status = StatusCancelled
		it.harness.Reason = "cancelled"
	case err != nil:
		it.status = StatusFailed
		it.errors = append(it.errors, Error{Code: "internal", Message: err.Error()})
	case res != nil:
		it.status = statusFor(res)
		it.harness.Reason = res.Reason
		it.harness.Text = res.Text
		it.harness.Result = res.Result
		it.harness.SubTurns = res.SubTurns
	default:
		it.status = StatusFailed
	}
	if it.usage.Harness != nil && res != nil {
		it.usage.Harness.SubTurns = res.SubTurns
	}
	it.mu.Unlock()

	if err != nil && !cancelled {
		s.notify(NotifyError, errorEvent{
			InteractionID: it.id,
			Error:         Error{Code: "internal", Message: err.Error()},
			EventType:     NotifyError,
		})
	}
}

// complete emits the last notification an interaction ever produces.
func (s *Server) complete(it *interaction) {
	s.notify(NotifyInteractionCompleted, interactionEnvelope{
		Interaction: it.snapshot(false), EventType: NotifyInteractionCompleted,
	})
}

// statusFor maps a run's terminal reason onto Google's status enum. A run
// that hit its sub-turn ceiling is `incomplete`, which is the same word
// Google uses for a generation cut short by a token cap; everything else that
// ended on its own terms is `completed`, with harness.reason carrying which
// way (internal/session, RunResult.Reason).
func statusFor(res *session.RunResult) string {
	if res.Reason == "max_sub_turns" {
		return StatusIncomplete
	}
	if res.Status == store.StatusFailed {
		return StatusFailed
	}
	return StatusCompleted
}

// snapshot builds the Interaction resource. While a run is still going the
// steps and usage come from its translator, so interactions.get on an
// in-progress interaction answers with what has happened so far rather than
// with nothing; once it has finished they are the values finish recorded.
func (it *interaction) snapshot(withSteps bool) Interaction {
	it.mu.Lock()
	defer it.mu.Unlock()
	if it.tr != nil {
		it.steps, it.usage = it.tr.snapshot()
	}
	out := Interaction{
		ID: it.id, Object: "interaction", Model: it.model, Status: it.status,
		Created: it.created.Format(time.RFC3339), Updated: it.updated.Format(time.RFC3339),
		Errors: it.errors,
	}
	if withSteps {
		out.Steps = it.steps
	}
	if it.usage.TotalTokens > 0 {
		u := it.usage
		out.Usage = &u
	}
	h := it.harness
	out.Harness = &h
	return out
}

// --- append, cancel, get, delete ---

func (s *Server) append(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var p AppendParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, errorf(CodeInvalidParams, "interactions.append params: %v", err)
	}
	it, ok := s.lookup(p.InteractionID)
	if !ok {
		return nil, errorf(CodeInteractionNotFound, "no interaction %q", p.InteractionID)
	}
	it.mu.Lock()
	status := it.status
	it.mu.Unlock()
	if status != StatusInProgress {
		return nil, errorf(CodeInteractionNotRunning, "interaction %s is %s; start a new interaction with previous_interaction_id set to it instead", it.id, status)
	}
	text, rerr := inputText(p.Input)
	if rerr != nil {
		return nil, rerr
	}
	if text == "" {
		return nil, errorf(CodeInvalidParams, "interactions.append needs some input")
	}

	appended, err := s.opts.Store.AppendEvents(ctx, it.sessionID, []store.EventInput{{
		Kind:    store.KindSteerMessage,
		Payload: store.SteerMessagePayload{Text: text, Source: "cli"},
	}})
	if err != nil {
		return nil, errorf(CodeInternalError, "record the input: %v", err)
	}
	if p.Harness != nil && p.Harness.MessageID != "" {
		s.rememberMessageID(it, appended[0].Seq, p.Harness.MessageID)
	}
	return AppendResult{InteractionID: it.id, Seq: appended[0].Seq}, nil
}

func (s *Server) cancelInteraction(params json.RawMessage) (any, *rpcError) {
	var p IDParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, errorf(CodeInvalidParams, "interactions.cancel params: %v", err)
	}
	it, ok := s.lookup(p.InteractionID)
	if !ok {
		return nil, errorf(CodeInteractionNotFound, "no interaction %q", p.InteractionID)
	}
	it.mu.Lock()
	cancel, status := it.cancel, it.status
	it.mu.Unlock()
	if status != StatusInProgress {
		// Cancelling something already finished is not an error: it is the
		// state the caller asked for, and a client racing a completion
		// should not have to handle both outcomes.
		return GetResult{Interaction: it.snapshot(true)}, nil
	}
	if cancel != nil {
		cancel()
	}
	<-it.done
	return GetResult{Interaction: it.snapshot(true)}, nil
}

func (s *Server) get(params json.RawMessage) (any, *rpcError) {
	var p IDParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, errorf(CodeInvalidParams, "interactions.get params: %v", err)
	}
	it, ok := s.lookup(p.InteractionID)
	if !ok {
		return nil, errorf(CodeInteractionNotFound, "no interaction %q", p.InteractionID)
	}
	return GetResult{Interaction: it.snapshot(true)}, nil
}

func (s *Server) delete(params json.RawMessage) (any, *rpcError) {
	var p IDParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, errorf(CodeInvalidParams, "interactions.delete params: %v", err)
	}
	s.mu.Lock()
	it, ok := s.interactions[p.InteractionID]
	if ok && s.running == it {
		s.mu.Unlock()
		return nil, errorf(CodeInteractionNotRunning, "interaction %s is still running; cancel it first", p.InteractionID)
	}
	delete(s.interactions, p.InteractionID)
	for i, id := range s.order {
		if id == p.InteractionID {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	s.mu.Unlock()
	if !ok {
		return nil, errorf(CodeInteractionNotFound, "no interaction %q", p.InteractionID)
	}
	// Google's delete answers with an empty body. The session's own
	// transcript on disk is untouched: this forgets the interaction, it does
	// not erase the run.
	return map[string]any{}, nil
}

// --- helpers ---

func (s *Server) lookup(id string) (*interaction, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.interactions[id]
	return it, ok
}

func (s *Server) rememberMessageID(it *interaction, seq int64, id string) {
	it.mu.Lock()
	tr := it.tr
	it.mu.Unlock()
	if tr != nil {
		tr.setMessageID(seq, id)
	}
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
	host := newHostTools(nil, "", nil)
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
func (s *Server) callFunction(ctx context.Context, p FunctionCallParams) (FunctionCallResult, error) {
	var res FunctionCallResult
	if err := s.conn.Call(ctx, MethodFunctionCall, p, &res); err != nil {
		return FunctionCallResult{}, err
	}
	return res, nil
}

// inputText flattens Google's polymorphic `input` into the instruction the
// loop takes. All four forms Google accepts are read: a bare string, one
// Content, an array of Content, and an array of Step.
func inputText(raw json.RawMessage) (string, *rpcError) {
	if len(raw) == 0 {
		return "", nil
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "null" {
		return "", nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", errorf(CodeInvalidParams, "input: %v", err)
		}
		return s, nil
	}
	if trimmed[0] == '{' {
		var c Content
		if err := json.Unmarshal(raw, &c); err != nil {
			return "", errorf(CodeInvalidParams, "input: %v", err)
		}
		return contentText([]Content{c})
	}
	// An array: Google allows an array of Content or an array of Step, and
	// the two are told apart element by element by the `type`
	// discriminator. Anything with a Content type is content; anything else
	// has to be a step.
	var raws []json.RawMessage
	if err := json.Unmarshal(raw, &raws); err != nil {
		return "", errorf(CodeInvalidParams, "input: %v", err)
	}
	var parts []string
	for _, el := range raws {
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(el, &probe); err != nil {
			return "", errorf(CodeInvalidParams, "input: %v", err)
		}
		switch probe.Type {
		case "text", "image":
			var c Content
			if err := json.Unmarshal(el, &c); err != nil {
				return "", errorf(CodeInvalidParams, "input: %v", err)
			}
			t, rerr := contentText([]Content{c})
			if rerr != nil {
				return "", rerr
			}
			parts = append(parts, t)
		case StepUserInput:
			var st Step
			if err := json.Unmarshal(el, &st); err != nil {
				return "", errorf(CodeInvalidParams, "input: %v", err)
			}
			t, rerr := contentText(st.Content)
			if rerr != nil {
				return "", rerr
			}
			parts = append(parts, t)
		default:
			return "", errorf(CodeUnsupported, "input element type %q: only text and image content, and %q steps, are accepted as input", probe.Type, StepUserInput)
		}
	}
	return strings.Join(parts, "\n\n"), nil
}

func contentText(blocks []Content) (string, *rpcError) {
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "image":
			// Not built. An image would have to be materialised into the
			// working directory for the loop to name it in the opening
			// message (internal/attachment), and writing into a directory
			// the client owns is a decision this protocol has not taken.
			return "", errorf(CodeUnsupported, "image input is not implemented: write the file into the working directory and name its path in the text instead")
		default:
			return "", errorf(CodeInvalidParams, "input content type %q", b.Type)
		}
	}
	return strings.Join(parts, "\n"), nil
}

// effortFrom maps Google's thinking_level onto the loop's effort. The two
// vocabularies already overlap on "low" and "high", and anything else passes
// through for internal/gemini to map (intent.go, thinkingLevelFromEffort).
func effortFrom(g *GenerationConfig) string {
	if g == nil || g.ThinkingLevel == "" {
		return wire.EffortHigh
	}
	return g.ThinkingLevel
}

// thinkingLevelOf is the level a create body asked for, empty when it named
// none.
func thinkingLevelOf(g *GenerationConfig) string {
	if g == nil {
		return ""
	}
	return g.ThinkingLevel
}

func maxTokensFrom(g *GenerationConfig) int {
	if g == nil {
		return 0
	}
	return g.MaxOutputTokens
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

func harnessInt(h *CreateHarness, get func(*CreateHarness) int) int {
	if h == nil {
		return 0
	}
	return get(h)
}
