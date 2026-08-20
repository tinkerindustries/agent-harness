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

// dialTimeout bounds one dial when the caller's context carries no deadline
// of its own (Manager.dial). 60s rather than the 30s a plain process start
// or TCP connect would need: the servers that motivated this — `uvx
// blender-mcp`, `uvx some-other-server` — resolve and install a package
// before they ever speak MCP on a cold start, and that routinely runs
// longer than a bare dial would.
const dialTimeout = 60 * time.Second

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
