package session

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/deepseek"
	"github.com/mrgeoffrich/agent-harness/internal/httplog"
	"github.com/mrgeoffrich/agent-harness/internal/store"
	"github.com/mrgeoffrich/agent-harness/internal/tools"
	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// keyNotOnDisk is the API key the wired test runner sends. It is fake, but
// it must still never appear in the captured log, decompressed.
const keyNotOnDisk = "sk-session-wired-test"

// newWiredTestRunner builds a runner whose client captures every exchange
// into rec, the way cmd/harness wires serve and run.
func newWiredTestRunner(t *testing.T, baseURL string, rec *httplog.Recorder) *Runner {
	t.Helper()
	r := newTestRunner(t, baseURL)
	r.Recorder = rec
	r.Client = deepseek.NewClient(baseURL, keyNotOnDisk, deepseek.WithTransportWrapper(func(next http.RoundTripper) http.RoundTripper {
		return httplog.NewTransport(next, rec)
	}))
	return r
}

// sessionHTTPLog locates a session's capture file, assuming the exchanges
// happened today.
func sessionHTTPLog(root, sessionID string) string {
	return filepath.Join(root, time.Now().Format("2006-01-02"), sessionID, "exchanges.jsonl.gz")
}

// readHTTPExchanges decodes every JSON line in a gzipped capture file.
func readHTTPExchanges(t *testing.T, path string) []httplog.Exchange {
	t.Helper()
	f, err := openGzip(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	var out []httplog.Exchange
	dec := json.NewDecoder(f)
	for {
		var ex httplog.Exchange
		if err := dec.Decode(&ex); err == io.EOF {
			return out
		} else if err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		out = append(out, ex)
	}
}

// decompress returns the uncompressed bytes of a gzip file.
func decompress(t *testing.T, path string) []byte {
	t.Helper()
	f, err := openGzip(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

func openGzip(path string) (io.ReadCloser, error) {
	raw, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return gzip.NewReader(raw)
}

// A run with a Recorder attached writes one capture file for that session
// id under the recorder's root, with at least one exchange, and the API key
// never reaches the decompressed bytes.
func TestRunWithRecorderWritesSessionHTTPLog(t *testing.T) {
	srv := plainAnswerServer(t, "all done")
	defer srv.Close()
	root := t.TempDir()
	rec := httplog.NewRecorder(root)
	r := newWiredTestRunner(t, srv.URL, rec)

	ws := t.TempDir()
	res, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "say something",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != store.StatusOK {
		t.Fatalf("expected status ok, got %s", res.Status)
	}

	path := sessionHTTPLog(root, res.SessionID)
	lines := readHTTPExchanges(t, path)
	if len(lines) < 1 {
		t.Fatalf("no exchanges written to %s", path)
	}
	for i, ex := range lines {
		if ex.SessionID != res.SessionID {
			t.Errorf("line %d has session_id %q, want %q", i, ex.SessionID, res.SessionID)
		}
	}
	for _, needle := range []string{keyNotOnDisk, "sk-"} {
		if bytes.Contains(decompress(t, path), []byte(needle)) {
			t.Errorf("decompressed log contains %q", needle)
		}
	}
}

// A nil Recorder is a no-op everywhere: the run proceeds normally and no
// capture file exists to find.
func TestRunWithNilRecorderRunsNormally(t *testing.T) {
	srv := plainAnswerServer(t, "all done")
	defer srv.Close()
	r := newTestRunner(t, srv.URL) // Recorder stays nil

	ws := t.TempDir()
	res, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "say something",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != store.StatusOK || res.Text != "all done" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

// WebFetch makes its own completion call on the tool-execution context, so
// the session id must reach it there: a run that calls WebFetch records
// every exchange — the sub-turn streams and the flash summariser's
// completion — under the session id.
func TestWebFetchCompletionIsAttributedToSession(t *testing.T) {
	var streamCall int32Counter
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/page" {
			fmt.Fprint(w, "<html><body>the page says hello</body></html>")
			return
		}
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &probe)
		if !probe.Stream {
			resp := wire.ChatCompletionResponse{
				Choices: []wire.Choice{{Message: wire.Message{Role: wire.RoleAssistant, Content: wire.TextContent("the answer")}, FinishReason: wire.FinishStop}},
				Usage:   &wire.Usage{PromptTokens: 50, CompletionTokens: 10},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		if streamCall.next() == 0 {
			args := fmt.Sprintf(`{"url":%q,"prompt":"what does the page say?"}`, srv.URL+"/page")
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{
					Role:      "assistant",
					ToolCalls: []wire.ToolCallDelta{{Index: 0, ID: "call_00_webfetch", Type: "function", Function: wire.ToolCallFuncDelta{Name: "WebFetch", Arguments: args}}},
				}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishToolCalls)}},
				Usage:   &wire.Usage{PromptTokens: 200, PromptCacheHitTokens: 100, PromptCacheMissTokens: 100, CompletionTokens: 5},
			})
		} else {
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{Role: "assistant", Content: strPtr("all done")}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishStop)}},
				Usage:   &wire.Usage{PromptTokens: 300, PromptCacheHitTokens: 100, PromptCacheMissTokens: 200, CompletionTokens: 5},
			})
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	root := t.TempDir()
	rec := httplog.NewRecorder(root)
	r := newWiredTestRunner(t, srv.URL, rec)

	ws := t.TempDir()
	res, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "fetch the page",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != store.StatusOK || res.Text != "all done" {
		t.Fatalf("unexpected result: %+v", res)
	}

	lines := readHTTPExchanges(t, sessionHTTPLog(root, res.SessionID))
	if len(lines) < 3 {
		t.Fatalf("got %d exchanges, want at least 3 (two streams plus the WebFetch completion)", len(lines))
	}
	for i, ex := range lines {
		if ex.SessionID != res.SessionID {
			t.Errorf("line %d has session_id %q, want %q", i, ex.SessionID, res.SessionID)
		}
	}
	var webfetchCompletion bool
	for _, ex := range lines {
		if strings.Contains(ex.ReqBody, "Answer the question using only the page content") {
			webfetchCompletion = true
			break
		}
	}
	if !webfetchCompletion {
		t.Error("no exchange carries the WebFetch summariser's completion request")
	}
}
