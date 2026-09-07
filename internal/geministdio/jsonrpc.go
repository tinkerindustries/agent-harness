package geministdio

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// JSON-RPC error codes. The first five are the standard set; the rest are
// this protocol's own and are listed in docs/STDIO-PROTOCOL.md.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603

	// CodeNotInitialized is returned for any method that arrives before the
	// initialize/initialized handshake completes.
	CodeNotInitialized = -32000
	// CodeInteractionNotFound names an interaction id this process has never
	// minted, or has already deleted.
	CodeInteractionNotFound = -32001
	// CodeInteractionNotRunning is returned by a method that only means
	// something for an interaction still in progress — cancel and append.
	CodeInteractionNotRunning = -32002
	// CodeCredentialsMissing is returned by interactions.create when no API
	// key reached this process.
	CodeCredentialsMissing = -32003
	// CodeUnsupported names a field of Google's create-interaction body that
	// this surface understands but cannot honour — an `agent` rather than a
	// `model`, a server-side tool it has no way to run.
	CodeUnsupported = -32004
)

// message is one JSON-RPC frame in either direction. `jsonrpc` is omitted on
// the wire, matching Codex's app-server framing, which the client this was
// built for already implements; a client that sends the header anyway is
// accepted, since the field is simply not read.
//
// Exactly one of Method (a request or a notification), Result, or Error is
// meaningful on any given frame. A frame with a Method and no ID is a
// notification and is never answered.
type message struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

// rpcError is a JSON-RPC error object. Data carries Google's own error shape
// ({"code": "...", "message": "..."}) where there is one, so a client has the
// vendor's string code as well as this protocol's numeric one.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("jsonrpc %d: %s", e.Code, e.Message) }

func errorf(code int, format string, args ...any) *rpcError {
	return &rpcError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// maxLine bounds one frame. A function result carrying a base64 image is the
// largest thing that crosses, so this is headroom rather than a working limit.
const maxLine = 32 << 20

// handler answers one client-initiated request. Returning a nil result and a
// nil error answers with JSON null, which is what the notification-shaped
// methods (initialized) never need and every request-shaped method avoids.
type handler func(ctx context.Context, method string, params json.RawMessage) (any, *rpcError)

// Conn is the framing layer: it reads frames off r, hands requests and
// notifications to a handler, writes answers and server-initiated traffic to
// w, and matches the client's answers to the requests this side sent.
//
// Every write goes through one mutex, so the order notifications are enqueued
// in is the order they reach the parent. That is the whole of the ordering
// guarantee docs/STDIO-PROTOCOL.md makes: step events for one interaction
// arrive in the order this process produced them.
type Conn struct {
	r io.Reader
	w io.Writer

	writeMu sync.Mutex
	enc     *json.Encoder

	// handle is set once, before Serve runs.
	handle handler

	// serial names the methods that must be handled on the read goroutine,
	// in arrival order, rather than on one of their own. The handshake is
	// the case: initialize and initialized are two frames a client sends
	// back to back, and handling them concurrently lets the acknowledgement
	// be processed before the request it acknowledges.
	serial func(method string) bool

	mu      sync.Mutex
	nextID  int64
	pending map[string]chan message
	closed  bool
}

// NewConn returns a Conn reading frames from r and writing them to w.
func NewConn(r io.Reader, w io.Writer, h handler, serial func(method string) bool) *Conn {
	return &Conn{r: r, w: w, enc: json.NewEncoder(w), handle: h, serial: serial, pending: map[string]chan message{}}
}

// Notify sends a notification: a frame with a method and no id, which the
// client must never answer and must ignore when it does not recognise it.
func (c *Conn) Notify(method string, params any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("geministdio: encode %s params: %w", method, err)
	}
	return c.write(message{Method: method, Params: raw})
}

// ErrConnClosed is returned by Call and Notify once the pipe has gone.
var ErrConnClosed = errors.New("geministdio: connection closed")

// Call sends a server-initiated request and waits for the client's answer.
// It is how a function tool the client declared gets executed: this side
// asks, the parent runs it, the parent answers.
//
// A client that does not implement the method must answer with a JSON-RPC
// error — the decline docs/STDIO-PROTOCOL.md requires — and never leave the
// request unanswered. An unanswered request would hold the sub-turn open
// until ctx expires, which for a tool call is the tool's own timeout.
func (c *Conn) Call(ctx context.Context, method string, params any, result any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("geministdio: encode %s params: %w", method, err)
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrConnClosed
	}
	c.nextID++
	id := fmt.Sprintf("h%d", c.nextID)
	ch := make(chan message, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	idRaw, _ := json.Marshal(id)
	if err := c.write(message{ID: idRaw, Method: method, Params: raw}); err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case reply, ok := <-ch:
		if !ok {
			return ErrConnClosed
		}
		if reply.Error != nil {
			return reply.Error
		}
		if result == nil || len(reply.Result) == 0 {
			return nil
		}
		if err := json.Unmarshal(reply.Result, result); err != nil {
			return fmt.Errorf("geministdio: decode %s result: %w", method, err)
		}
		return nil
	}
}

func (c *Conn) write(m message) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return ErrConnClosed
	}
	if err := c.enc.Encode(m); err != nil {
		return fmt.Errorf("geministdio: write frame: %w", err)
	}
	return nil
}

// Serve reads frames until the reader ends or ctx is cancelled. Reaching the
// end of stdin is the parent saying the session is over, so Serve returns nil
// for a clean EOF; a decode failure is reported to the client and the frame
// skipped, because one malformed line is not a reason to end a session.
//
// Each client request runs on its own goroutine — except the ones serial
// names — so a long-running interactions.create does not stop
// interactions.cancel or an append from being read. Serialisation of anything that needs it belongs to the handler,
// not here.
func (c *Conn) Serve(ctx context.Context) error {
	defer c.shutdown()

	sc := bufio.NewScanner(c.r)
	sc.Buffer(make([]byte, 0, 64*1024), maxLine)

	var wg sync.WaitGroup
	defer wg.Wait()

	for sc.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var m message
		if err := json.Unmarshal(line, &m); err != nil {
			_ = c.write(message{Error: errorf(CodeParseError, "malformed JSON frame: %v", err)})
			continue
		}
		switch {
		case m.Method == "" && len(m.ID) > 0:
			// An answer to something this side asked.
			c.deliver(m)
		case m.Method == "":
			// Neither a method nor an id: nothing to do with it, and not
			// worth ending a session over.
			continue
		case c.serial != nil && c.serial(m.Method):
			c.dispatch(ctx, m)
		default:
			wg.Add(1)
			go func(m message) {
				defer wg.Done()
				c.dispatch(ctx, m)
			}(m)
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("geministdio: read stdin: %w", err)
	}
	return nil
}

func (c *Conn) dispatch(ctx context.Context, m message) {
	result, rerr := c.handle(ctx, m.Method, m.Params)
	if len(m.ID) == 0 {
		// A notification. An unrecognised one is ignored rather than
		// answered, and even a real failure has nowhere to go.
		return
	}
	if rerr != nil {
		_ = c.write(message{ID: m.ID, Error: rerr})
		return
	}
	raw, err := json.Marshal(result)
	if err != nil {
		_ = c.write(message{ID: m.ID, Error: errorf(CodeInternalError, "encode %s result: %v", m.Method, err)})
		return
	}
	_ = c.write(message{ID: m.ID, Result: raw})
}

func (c *Conn) deliver(m message) {
	var id string
	if err := json.Unmarshal(m.ID, &id); err != nil {
		// Numeric ids are legal JSON-RPC; this side only ever mints string
		// ones, so anything else answers a request that cannot be ours.
		return
	}
	c.mu.Lock()
	ch, ok := c.pending[id]
	if ok {
		delete(c.pending, id)
	}
	c.mu.Unlock()
	if ok {
		ch <- m
	}
}

// shutdown marks the connection closed and releases everything waiting on an
// answer that will now never come. A tool call blocked in Call returns
// ErrConnClosed and fails its own tool result rather than the whole run.
func (c *Conn) shutdown() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	pending := c.pending
	c.pending = map[string]chan message{}
	c.mu.Unlock()
	for _, ch := range pending {
		close(ch)
	}
}
