package mcpclient

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

// clientVersion is the Implementation.Version every dial reports to the
// server it connects to, alongside the fixed name "deepseek-harness".
const clientVersion = "0.1.0"

// dialTimeout bounds one dial (Manager.dial). Minutes, not the 30s a plain
// process start or TCP connect would need, for two reasons that compound on
// a first run: a server launched as `uvx <package>` or `npx <package>`
// resolves and installs before it ever speaks MCP, and a server that drives
// an external application may talk to it during startup — one observed
// server spent forty seconds per handshake attempt against an application
// that answered slowly, before it would answer tools/list at all.
//
// It is applied even when the caller already has a deadline, capped to
// whichever is shorter, and it is deliberately shorter than
// internal/httpapi's probe budget. That ordering is what makes a stuck dial
// diagnosable: the inner bound fires first and reports what the dial was
// waiting for, instead of the outer one firing and leaving a bare "context
// deadline exceeded" on the row.
const dialTimeout = 3 * time.Minute

// defaultDial is the real dialer Manager.dial falls back to when Dial is
// nil: stdio via mcp.CommandTransport, http via
// mcp.StreamableClientTransport (docs/MCP.md, "Probing").
func defaultDial(ctx context.Context, srv store.MCPServer) (*mcpsdk.ClientSession, error) {
	impl := &mcpsdk.Implementation{Name: "deepseek-harness", Version: clientVersion}
	client := mcpsdk.NewClient(impl, nil)

	switch srv.Transport {
	case store.MCPTransportStdio:
		cmd := exec.CommandContext(ctx, srv.Command, srv.Args...)
		// Appended to, not replacing, the harness's own environment: a
		// server launched as `uvx` or `npx` needs PATH and HOME to find its
		// runtime and its own cache, not just the handful of variables the
		// operator configured for it.
		cmd.Env = append(os.Environ(), envPairs(srv.Env)...)
		cmd.Stderr = &stderrLogger{server: srv.Name}
		return client.Connect(ctx, &mcpsdk.CommandTransport{Command: cmd}, nil)

	case store.MCPTransportHTTP:
		hc := &http.Client{
			Transport: &headerRoundTripper{headers: srv.Headers, base: http.DefaultTransport},
		}
		return client.Connect(ctx, &mcpsdk.StreamableClientTransport{
			Endpoint:   srv.URL,
			HTTPClient: hc,
		}, nil)

	default:
		// ValidateMCPServer rejects any other transport before a row can
		// ever be saved, so this is a defensive error rather than a path a
		// well-formed row can reach.
		return nil, fmt.Errorf("mcpclient: server %q: unknown transport %q", srv.Name, srv.Transport)
	}
}

// envPairs renders env as NAME=VALUE strings for exec.Cmd.Env.
func envPairs(env map[string]string) []string {
	pairs := make([]string, 0, len(env))
	for k, v := range env {
		pairs = append(pairs, k+"="+v)
	}
	return pairs
}

// headerRoundTripper adds a server's configured HTTP headers to every
// request the streamable-HTTP transport sends, without mutating the
// *http.Request the caller handed it.
type headerRoundTripper struct {
	headers map[string]string
	base    http.RoundTripper
}

func (h *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}
	base := h.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// stderrLogger is an io.Writer that logs each line an MCP server's stderr
// produces through the harness's own log, prefixed with the server's name,
// so a stdio server that dies on startup says why in the same place every
// other harness log line goes rather than vanishing with the subprocess.
//
// It buffers instead of assuming Write delivers whole lines — os/exec
// forwards a child's stderr in whatever chunks the pipe hands it — and logs
// only complete lines, holding a trailing partial line until the next Write
// or, if the process exits without a final newline, dropping it: losing the
// last unterminated fragment of a dying process's output is an acceptable
// trade for never needing an explicit Close a caller could forget.
type stderrLogger struct {
	server string

	mu  sync.Mutex
	buf []byte
}

func (w *stderrLogger) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := bytes.TrimRight(w.buf[:i], "\r")
		if len(line) > 0 {
			log.Printf("mcpclient: %s: %s", w.server, line)
		}
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}
