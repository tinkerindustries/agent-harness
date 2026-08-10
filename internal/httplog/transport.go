package httplog

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
)

// NewTransport wraps next so every request is recorded by rec. The
// returned RoundTripper is safe for concurrent use by many sessions at
// once. Whatever next returns, response or error, reaches the caller
// unchanged; a failure to write the log is logged and does not fail the
// request.
func NewTransport(next http.RoundTripper, rec *Recorder) http.RoundTripper {
	return &transport{next: next, rec: rec, attempts: make(map[string]*attemptState)}
}

type transport struct {
	next http.RoundTripper
	rec  *Recorder

	mu       sync.Mutex
	attempts map[string]*attemptState
}

// attemptState remembers a session's most recent request so a retry can be
// numbered. Client.do reissues the same method, URL, and body for a retry,
// and nothing else in the harness repeats a request byte-identically.
type attemptState struct {
	method  string
	url     string
	body    string
	attempt int
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	sessionID := SessionIDFromContext(req.Context())
	if sessionID == "" {
		sessionID = "harness"
	}

	reqBody, err := readRequestBody(req)
	if err != nil {
		log.Printf("httplog: read request body for %s: %v", sessionID, err)
	}

	ex := &Exchange{
		Attempt:    t.nextAttempt(sessionID, req, reqBody),
		SessionID:  sessionID,
		Method:     req.Method,
		URL:        req.URL.String(),
		ReqHeaders: redactedHeaders(req.Header),
		ReqBody:    reqBody,
		StartedAt:  time.Now(),
	}

	resp, err := t.next.RoundTrip(req)
	if err != nil {
		ex.Error = err.Error()
		ex.TotalMs = time.Since(ex.StartedAt).Milliseconds()
		if rerr := t.rec.record(ex); rerr != nil {
			log.Printf("httplog: record exchange for %s: %v", sessionID, rerr)
		}
		return nil, err
	}

	ex.Status = resp.StatusCode
	ex.RespHeaders = redactedHeaders(resp.Header)
	ex.TTFBMs = time.Since(ex.StartedAt).Milliseconds()
	resp.Body = &captureBody{src: resp.Body, rec: t.rec, ex: ex, ctx: req.Context()}
	return resp, nil
}

// nextAttempt returns the retry number for req: 0 for a new request, one
// more than the previous attempt for a request identical to the one before
// it in the same session.
func (t *transport) nextAttempt(sessionID string, req *http.Request, body string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.attempts[sessionID]
	if st == nil {
		st = &attemptState{}
		t.attempts[sessionID] = st
	}
	if st.method == req.Method && st.url == req.URL.String() && st.body == body {
		st.attempt++
		return st.attempt
	}
	st.method = req.Method
	st.url = req.URL.String()
	st.body = body
	st.attempt = 0
	return 0
}

// readRequestBody returns the request body for the record, leaving the
// request's own body intact: via GetBody when the request carries one,
// otherwise by reading it once and restoring a fresh reader in its place.
func readRequestBody(req *http.Request) (string, error) {
	if req.Body == nil {
		return "", nil
	}
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return "", err
		}
		defer body.Close()
		b, err := io.ReadAll(body)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, req.Body); err != nil {
		return "", err
	}
	req.Body.Close()
	req.Body = io.NopCloser(bytes.NewReader(buf.Bytes()))
	return buf.String(), nil
}

// captureBody tees the response body into the exchange record as the
// caller reads it and finalizes the line at EOF or Close. Buffering the
// whole body before returning would stall a streaming completion and trip
// the caller's idle watchdog (docs/DESIGN.md §4.3).
type captureBody struct {
	src io.ReadCloser
	rec *Recorder
	ex  *Exchange
	ctx context.Context

	done sync.Once
	mu   sync.Mutex
	buf  bytes.Buffer
}

func (c *captureBody) Read(p []byte) (int, error) {
	n, err := c.src.Read(p)
	if n > 0 {
		c.mu.Lock()
		c.buf.Write(p[:n])
		c.mu.Unlock()
	}
	if err == io.EOF {
		c.finalize(nil)
	} else if err != nil {
		c.finalize(err)
	}
	return n, err
}

func (c *captureBody) Close() error {
	err := c.src.Close()
	if c.ctx.Err() != nil {
		// The caller cancelled and stopped reading; the line still gets
		// written, with the body read so far and the cancellation recorded.
		c.finalize(c.ctx.Err())
	} else {
		c.finalize(nil)
	}
	return err
}

// finalize writes the exchange line exactly once, whenever the body ends.
func (c *captureBody) finalize(readErr error) {
	c.done.Do(func() {
		c.mu.Lock()
		c.ex.RespBody = c.buf.String()
		c.mu.Unlock()
		c.ex.TotalMs = time.Since(c.ex.StartedAt).Milliseconds()
		if readErr != nil {
			c.ex.Error = readErr.Error()
		}
		if err := c.rec.record(c.ex); err != nil {
			log.Printf("httplog: record exchange for %s: %v", c.ex.SessionID, err)
		}
	})
}
