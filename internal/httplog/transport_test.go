package httplog

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// sessionLogPath locates a session's log file under root, assuming the
// exchange happened today.
func sessionLogPath(root, sessionID string) string {
	return filepath.Join(root, time.Now().Format("2006-01-02"), sessionID, "exchanges.jsonl.gz")
}

// readExchanges decodes every JSON line in a gzipped log file.
func readExchanges(t *testing.T, path string) []Exchange {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip open %s: %v", path, err)
	}
	defer zr.Close()
	var out []Exchange
	dec := json.NewDecoder(zr)
	for {
		var ex Exchange
		if err := dec.Decode(&ex); err == io.EOF {
			return out
		} else if err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		out = append(out, ex)
	}
}

// logPaths returns every exchanges.jsonl.gz file under root.
func logPaths(t *testing.T, root string) []string {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && filepath.Base(path) == "exchanges.jsonl.gz" {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return paths
}

// doRequest sends req through c and fails the test if the request errors.
func doRequest(t *testing.T, c *http.Client, req *http.Request) *http.Response {
	t.Helper()
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

func TestExchangeRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if string(body) != `{"prompt":"hello"}` {
			t.Errorf("server saw request body %q", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[]}`))
	}))
	defer srv.Close()

	root := t.TempDir()
	rec := NewRecorder(root)
	client := &http.Client{Transport: NewTransport(srv.Client().Transport, rec)}

	req, err := http.NewRequestWithContext(WithSessionID(context.Background(), "sess-rt"), http.MethodPost, srv.URL+"/chat/completions", strings.NewReader(`{"prompt":"hello"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp := doRequest(t, client, req)
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	resp.Body.Close()
	if err := rec.Close("sess-rt"); err != nil {
		t.Fatalf("close recorder: %v", err)
	}

	lines := readExchanges(t, sessionLogPath(root, "sess-rt"))
	if len(lines) != 1 {
		t.Fatalf("got %d exchange lines, want 1", len(lines))
	}
	ex := lines[0]
	if ex.Seq != 1 {
		t.Errorf("Seq = %d, want 1", ex.Seq)
	}
	if ex.SessionID != "sess-rt" {
		t.Errorf("SessionID = %q, want %q", ex.SessionID, "sess-rt")
	}
	if ex.Method != http.MethodPost {
		t.Errorf("Method = %q, want POST", ex.Method)
	}
	if !strings.HasSuffix(ex.URL, "/chat/completions") {
		t.Errorf("URL = %q, want suffix /chat/completions", ex.URL)
	}
	if ex.Status != http.StatusOK {
		t.Errorf("Status = %d, want 200", ex.Status)
	}
	if ex.ReqBody != `{"prompt":"hello"}` {
		t.Errorf("ReqBody = %q, want the request body", ex.ReqBody)
	}
	if string(respBody) != `{"id":"x","choices":[]}` || ex.RespBody != `{"id":"x","choices":[]}` {
		t.Errorf("RespBody = %q, want %q", ex.RespBody, respBody)
	}
	if ex.Error != "" {
		t.Errorf("Error = %q, want empty", ex.Error)
	}
}

// The Authorization header carries the API key and must never reach disk,
// in the record or in the raw file bytes, no matter how the deflate stream
// chooses to encode them. The outbound request keeps its real values.
// Gemini's x-goog-api-key is the second credential header this harness
// sends, so it is pinned here too: redaction is by the named set in
// exchange.go, matched case-insensitively, and a new client's credential
// header failing to join that set fails this test verbatim.
func TestTransportRedactsCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test-secret" {
			t.Errorf("outbound Authorization = %q, want the real value", got)
		}
		if got := r.Header.Get("Cookie"); got != "session=abc123" {
			t.Errorf("outbound Cookie = %q, want the real value", got)
		}
		if got := r.Header.Get("X-Goog-Api-Key"); got != "AIzaSy-test-secret" {
			t.Errorf("outbound X-Goog-Api-Key = %q, want the real value", got)
		}
		w.Header().Set("Set-Cookie", "session=abc123")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	root := t.TempDir()
	rec := NewRecorder(root)
	client := &http.Client{Transport: NewTransport(srv.Client().Transport, rec)}

	req, err := http.NewRequestWithContext(WithSessionID(context.Background(), "sess-redact"), http.MethodGet, srv.URL+"/models", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer sk-test-secret")
	req.Header.Set("Proxy-Authorization", "Basic c2VjcmV0")
	req.Header.Set("Cookie", "session=abc123")
	// Mixed case on purpose: redaction is matched case-insensitively, the
	// way the Gemini client's lowercase "x-goog-api-key" still has to be
	// caught.
	req.Header.Set("X-Goog-Api-Key", "AIzaSy-test-secret")
	resp := doRequest(t, client, req)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if err := rec.Close("sess-redact"); err != nil {
		t.Fatalf("close recorder: %v", err)
	}

	ex := readExchanges(t, sessionLogPath(root, "sess-redact"))[0]
	for _, name := range []string{"Authorization", "Proxy-Authorization", "Cookie", "X-Goog-Api-Key"} {
		if got := ex.ReqHeaders[name]; got != "[redacted]" {
			t.Errorf("ReqHeaders[%s] = %q, want [redacted]", name, got)
		}
	}
	if got := ex.RespHeaders["Set-Cookie"]; got != "[redacted]" {
		t.Errorf("RespHeaders[Set-Cookie] = %q, want [redacted]", got)
	}

	raw, err := os.ReadFile(sessionLogPath(root, "sess-redact"))
	if err != nil {
		t.Fatalf("read raw log: %v", err)
	}
	for _, needle := range []string{"sk-test-secret", "sk-", "abc123", "AIzaSy-test-secret", "AIzaSy"} {
		if bytes.Contains(raw, []byte(needle)) {
			t.Errorf("raw log bytes contain %q", needle)
		}
	}
}

// The tee must pass reads straight through: the caller sees the first SSE
// frame before the handler writes the second. Buffering the body before
// returning would stall the stream and deadlock this test, which is why a
// timeout fails it instead of hanging.
func TestStreamingFlowsWithoutBuffering(t *testing.T) {
	writeSecond := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("handler: response writer is not a flusher")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"first\"}}]}\n\n")
		flusher.Flush()
		<-writeSecond
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"second\"}}]}\n\n")
		flusher.Flush()
	}))
	defer srv.Close()

	root := t.TempDir()
	rec := NewRecorder(root)
	client := &http.Client{Transport: NewTransport(srv.Client().Transport, rec)}

	req, err := http.NewRequestWithContext(WithSessionID(context.Background(), "sess-stream"), http.MethodPost, srv.URL+"/chat/completions", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp := doRequest(t, client, req)

	reader := bufio.NewReader(resp.Body)
	first := make(chan error, 1)
	go func() {
		_, err := reader.ReadString('\n')
		first <- err
	}()
	select {
	case err := <-first:
		if err != nil {
			t.Fatalf("read first frame: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first frame did not arrive before the handler wrote the second; the tee buffered the body")
	}
	writeSecond <- struct{}{}
	if _, err := io.ReadAll(reader); err != nil {
		t.Fatalf("read rest of stream: %v", err)
	}
	resp.Body.Close()
	if err := rec.Close("sess-stream"); err != nil {
		t.Fatalf("close recorder: %v", err)
	}

	ex := readExchanges(t, sessionLogPath(root, "sess-stream"))[0]
	if !strings.Contains(ex.RespBody, "first") || !strings.Contains(ex.RespBody, "second") {
		t.Errorf("RespBody %q is missing a frame", ex.RespBody)
	}
	if ex.Status != http.StatusOK {
		t.Errorf("Status = %d, want 200", ex.Status)
	}
	if ex.Error != "" {
		t.Errorf("Error = %q, want empty", ex.Error)
	}
}

// Closing the body before the stream ends still writes the line, with the
// body read so far and no error: nothing was cancelled.
func TestEarlyCloseWritesPartialBody(t *testing.T) {
	release := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"first\"}}]}\n\n")
		flusher.Flush()
		<-release
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"second\"}}]}\n\n")
		flusher.Flush()
	}))
	defer srv.Close()

	root := t.TempDir()
	rec := NewRecorder(root)
	client := &http.Client{Transport: NewTransport(srv.Client().Transport, rec)}

	req, err := http.NewRequestWithContext(WithSessionID(context.Background(), "sess-early"), http.MethodPost, srv.URL+"/chat/completions", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp := doRequest(t, client, req)

	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read first frame: %v", err)
	}
	if !strings.Contains(line, "first") {
		t.Fatalf("first frame = %q", line)
	}
	resp.Body.Close()
	release <- struct{}{}
	if err := rec.Close("sess-early"); err != nil {
		t.Fatalf("close recorder: %v", err)
	}

	ex := readExchanges(t, sessionLogPath(root, "sess-early"))[0]
	if !strings.Contains(ex.RespBody, "first") {
		t.Errorf("RespBody %q is missing the frame read before close", ex.RespBody)
	}
	if strings.Contains(ex.RespBody, "second") {
		t.Errorf("RespBody %q holds the frame read after close", ex.RespBody)
	}
	if ex.Error != "" {
		t.Errorf("Error = %q, want empty: nothing was cancelled", ex.Error)
	}
}

// Closing the body because the caller cancelled still writes the line, and
// the cancellation is what Error records.
func TestCancelledEarlyCloseRecordsError(t *testing.T) {
	release := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"first\"}}]}\n\n")
		flusher.Flush()
		<-release
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"second\"}}]}\n\n")
		flusher.Flush()
	}))
	defer srv.Close()

	root := t.TempDir()
	rec := NewRecorder(root)
	client := &http.Client{Transport: NewTransport(srv.Client().Transport, rec)}

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(WithSessionID(ctx, "sess-cancel"), http.MethodPost, srv.URL+"/chat/completions", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp := doRequest(t, client, req)

	reader := bufio.NewReader(resp.Body)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatalf("read first frame: %v", err)
	}
	cancel()
	resp.Body.Close()
	release <- struct{}{}
	if err := rec.Close("sess-cancel"); err != nil {
		t.Fatalf("close recorder: %v", err)
	}

	ex := readExchanges(t, sessionLogPath(root, "sess-cancel"))[0]
	if !strings.Contains(ex.RespBody, "first") {
		t.Errorf("RespBody %q is missing the frame read before close", ex.RespBody)
	}
	if strings.Contains(ex.RespBody, "second") {
		t.Errorf("RespBody %q holds the frame read after close", ex.RespBody)
	}
	if ex.Error == "" {
		t.Error("Error is empty, want the cancellation recorded")
	}
}

// A retry is its own line, not an amendment to the previous one: two
// byte-identical requests from one session number attempts 0 and 1 and each
// records its own status.
func TestRetryIsItsOwnLine(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	root := t.TempDir()
	rec := NewRecorder(root)
	client := &http.Client{Transport: NewTransport(srv.Client().Transport, rec)}

	for i := 0; i < 2; i++ {
		req, err := http.NewRequestWithContext(WithSessionID(context.Background(), "sess-retry"), http.MethodPost, srv.URL+"/chat/completions", strings.NewReader(`{"prompt":"hi"}`))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		resp := doRequest(t, client, req)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	if err := rec.Close("sess-retry"); err != nil {
		t.Fatalf("close recorder: %v", err)
	}

	lines := readExchanges(t, sessionLogPath(root, "sess-retry"))
	if len(lines) != 2 {
		t.Fatalf("got %d exchange lines, want 2", len(lines))
	}
	if lines[0].Attempt != 0 || lines[1].Attempt != 1 {
		t.Errorf("attempts = %d, %d; want 0, 1", lines[0].Attempt, lines[1].Attempt)
	}
	if lines[0].Status != http.StatusServiceUnavailable || lines[1].Status != http.StatusOK {
		t.Errorf("statuses = %d, %d; want 503, 200", lines[0].Status, lines[1].Status)
	}
}

// Closing a session drops its retry identity with the writer: reopening the
// same session id starts again at attempt 0 for a request that would
// otherwise have counted as a retry.
func TestAttemptStateDroppedOnClose(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	root := t.TempDir()
	rec := NewRecorder(root)
	client := &http.Client{Transport: NewTransport(srv.Client().Transport, rec)}

	send := func() {
		req, err := http.NewRequestWithContext(WithSessionID(context.Background(), "sess-again"), http.MethodPost, srv.URL+"/chat/completions", strings.NewReader(`{"prompt":"same"}`))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		resp := doRequest(t, client, req)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	send()
	send()
	if err := rec.Close("sess-again"); err != nil {
		t.Fatalf("close recorder: %v", err)
	}
	send()
	if err := rec.Close("sess-again"); err != nil {
		t.Fatalf("close recorder: %v", err)
	}

	lines := readExchanges(t, sessionLogPath(root, "sess-again"))
	if len(lines) != 3 {
		t.Fatalf("got %d exchange lines, want 3", len(lines))
	}
	if lines[0].Attempt != 0 || lines[1].Attempt != 1 || lines[2].Attempt != 0 {
		t.Errorf("attempts = %d, %d, %d; want 0, 1, 0", lines[0].Attempt, lines[1].Attempt, lines[2].Attempt)
	}
}

// A transport error with no response at all reaches the caller unchanged
// and is still recorded, with the error and no status.
func TestTransportErrorIsRecorded(t *testing.T) {
	root := t.TempDir()
	rec := NewRecorder(root)
	client := &http.Client{Transport: NewTransport(errRoundTripper{errors.New("boom")}, rec)}

	req, err := http.NewRequestWithContext(WithSessionID(context.Background(), "sess-err"), http.MethodGet, "http://example.invalid/x", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := client.Do(req)
	if err == nil {
		t.Fatal("client.Do returned nil error, want the transport error")
	}
	if resp != nil {
		t.Fatalf("resp = %v, want nil", resp)
	}
	if err := rec.Close("sess-err"); err != nil {
		t.Fatalf("close recorder: %v", err)
	}

	ex := readExchanges(t, sessionLogPath(root, "sess-err"))[0]
	if ex.Error != "boom" {
		t.Errorf("Error = %q, want %q", ex.Error, "boom")
	}
	if ex.Status != 0 {
		t.Errorf("Status = %d, want 0", ex.Status)
	}
	if ex.Method != http.MethodGet || ex.URL != "http://example.invalid/x" {
		t.Errorf("recorded %s %s", ex.Method, ex.URL)
	}
}

type errRoundTripper struct{ err error }

func (e errRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, e.err
}

// Traffic whose context carries no session id lands under "harness", so
// the CLI's balance and models calls are part of the log.
func TestUnattributedTrafficLandsUnderHarness(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	root := t.TempDir()
	rec := NewRecorder(root)
	client := &http.Client{Transport: NewTransport(srv.Client().Transport, rec)}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/user/balance", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp := doRequest(t, client, req)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if err := rec.Close("harness"); err != nil {
		t.Fatalf("close recorder: %v", err)
	}

	ex := readExchanges(t, sessionLogPath(root, "harness"))[0]
	if ex.SessionID != "harness" {
		t.Errorf("SessionID = %q, want %q", ex.SessionID, "harness")
	}
	if ex.URL != srv.URL+"/user/balance" {
		t.Errorf("URL = %q, want %q", ex.URL, srv.URL+"/user/balance")
	}
}

// Many sessions recording at once must each get their own file with their
// own numbering, and no line may cross a file boundary. Run under -race.
func TestConcurrentSessionsWriteSeparateFiles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	root := t.TempDir()
	rec := NewRecorder(root)
	client := &http.Client{Transport: NewTransport(srv.Client().Transport, rec)}

	const sessions = 8
	const perSession = 5
	var wg sync.WaitGroup
	for i := 0; i < sessions; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sid := fmt.Sprintf("session-%02d", i)
			for j := 0; j < perSession; j++ {
				req, err := http.NewRequestWithContext(WithSessionID(context.Background(), sid), http.MethodGet, srv.URL+"/x", nil)
				if err != nil {
					t.Errorf("session %s: new request: %v", sid, err)
					return
				}
				resp, err := client.Do(req)
				if err != nil {
					t.Errorf("session %s: do: %v", sid, err)
					return
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}(i)
	}
	wg.Wait()
	if err := rec.CloseAll(); err != nil {
		t.Fatalf("close all: %v", err)
	}

	paths := logPaths(t, root)
	if len(paths) != sessions {
		t.Fatalf("got %d log files, want %d", len(paths), sessions)
	}
	for _, p := range paths {
		sessionID := filepath.Base(filepath.Dir(p))
		lines := readExchanges(t, p)
		if len(lines) != perSession {
			t.Errorf("%s: %d lines, want %d", sessionID, len(lines), perSession)
		}
		for i, ex := range lines {
			if ex.SessionID != sessionID {
				t.Errorf("%s: line %d has session_id %q", sessionID, i, ex.SessionID)
			}
			if ex.Seq != int64(i+1) {
				t.Errorf("%s: line %d has seq %d, want %d", sessionID, i, ex.Seq, i+1)
			}
		}
	}
}
