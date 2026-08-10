package session

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mrgeoffrich/deepseek-harness/internal/deepseek"
	"github.com/mrgeoffrich/deepseek-harness/internal/httplog"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
	"github.com/mrgeoffrich/deepseek-harness/internal/tools"
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
		Model: "test-model", Effort: deepseek.EffortHigh, Thinking: true, MaxTokens: 4000,
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
		Model: "test-model", Effort: deepseek.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "say something",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != store.StatusOK || res.Text != "all done" {
		t.Fatalf("unexpected result: %+v", res)
	}
}
