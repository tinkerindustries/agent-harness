// Package httplog captures the harness's raw HTTP traffic to the DeepSeek
// API and writes it to disk as gzipped JSON lines, one file per agent
// session under root/<day>/<session-id>/exchanges.jsonl.gz. Every outbound
// call funnels through Client.do, so the capture point is a RoundTripper
// wrapped around the client's transport; the session id rides on the
// request context. Nothing in this package knows about sessions, tools, or
// storage: it records what the process sent and received, and redacts the
// credentials out of it.
package httplog

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Recorder owns one open log file per session and is safe for concurrent
// use by many sessions at once.
type Recorder struct {
	root string

	mu      sync.Mutex
	writers map[string]*sessionWriter
}

// sessionWriter is one session's gzip file. The gzip writer is not safe
// for concurrent use, so every write, flush, and close takes mu. It also
// holds the session's retry identity: the previous request's method, URL,
// and body hash, so a retry can be numbered. The body is kept as a hash,
// not the body itself — a chat completion body is the whole message array,
// and a long-lived serve process would otherwise retain hundreds of
// megabytes it will never use again. Recorder.Close drops this state with
// the writer.
type sessionWriter struct {
	mu   sync.Mutex
	file *os.File
	gz   *gzip.Writer
	seq  int64

	method   string
	url      string
	bodyHash [sha256.Size]byte
	attempt  int
}

// NewRecorder returns a Recorder writing under root.
func NewRecorder(root string) *Recorder {
	return &Recorder{root: root, writers: make(map[string]*sessionWriter)}
}

// Open ensures a writer for sessionID exists under root/<day>, creating the
// directory and file if needed. Reopening an already-open session is a
// no-op and never truncates the file; the file is opened in append mode so
// an existing file is extended, not rewritten.
func (r *Recorder) Open(sessionID string, day time.Time) error {
	_, err := r.open(sessionID, day)
	return err
}

// open returns the writer for sessionID, creating it if absent. The map
// lock guards the writers map only: one session's slow write holds that
// session's own lock, so it never blocks another session opening.
func (r *Recorder) open(sessionID string, day time.Time) (*sessionWriter, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if w, ok := r.writers[sessionID]; ok {
		return w, nil
	}
	dir := filepath.Join(r.root, day.Format("2006-01-02"), sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "exchanges.jsonl.gz"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	w := &sessionWriter{file: f, gz: gzip.NewWriter(f)}
	r.writers[sessionID] = w
	return w, nil
}

// Close flushes and closes sessionID's gzip stream so the file ends as a
// valid gzip member. Closing a session that is not open is a no-op.
func (r *Recorder) Close(sessionID string) error {
	r.mu.Lock()
	w := r.writers[sessionID]
	delete(r.writers, sessionID)
	r.mu.Unlock()
	if w == nil {
		return nil
	}
	return w.close()
}

// NextAttempt returns the retry number for a request from sessionID: 0 for
// a new request, one more than the previous attempt for a request identical
// to the one before it in that session. The state lives on the session's
// writer, so Recorder.Close drops it with the file; a writer that cannot be
// opened is logged and the request counts as a new one, never a reason to
// fail the request itself.
func (r *Recorder) NextAttempt(sessionID, method, url string, bodyHash [sha256.Size]byte) int {
	w, err := r.open(sessionID, time.Now())
	if err != nil {
		log.Printf("httplog: open writer for %s: %v", sessionID, err)
		return 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.method == method && w.url == url && w.bodyHash == bodyHash {
		w.attempt++
		return w.attempt
	}
	w.method = method
	w.url = url
	w.bodyHash = bodyHash
	w.attempt = 0
	return 0
}

// CloseAll flushes and closes every open session writer.
func (r *Recorder) CloseAll() error {
	r.mu.Lock()
	ws := r.writers
	r.writers = make(map[string]*sessionWriter)
	r.mu.Unlock()
	var firstErr error
	for _, w := range ws {
		if err := w.close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// close flushes and closes the gzip stream and its file.
func (w *sessionWriter) close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.gz.Close(); err != nil {
		w.file.Close()
		return err
	}
	return w.file.Close()
}

// record appends ex as one JSON line to its session's log, opening the
// writer on demand with the exchange's start time picking the day. The
// sequence number is assigned here, under the session's write lock, so
// concurrent exchanges for one session number in arrival order.
func (r *Recorder) record(ex *Exchange) error {
	w, err := r.open(ex.SessionID, ex.StartedAt)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.seq++
	ex.Seq = w.seq
	line, err := json.Marshal(ex)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if _, err := w.gz.Write(line); err != nil {
		return err
	}
	// Flush after every exchange, not only at Close, so a file left unclosed
	// by a crash stays readable up to the last completed exchange.
	return w.gz.Flush()
}
