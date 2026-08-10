package httplog

import (
	"path/filepath"
	"testing"
	"time"
)

// A session reopened while open must keep the first writer: the second Open
// is a no-op, so what the first call wrote stays in the file and the
// sequence continues rather than restarting.
func TestOpenTwiceDoesNotTruncate(t *testing.T) {
	root := t.TempDir()
	rec := NewRecorder(root)
	day := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)

	if err := rec.Open("sess-double", day); err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := rec.record(&Exchange{SessionID: "sess-double", Method: "GET", URL: "http://a/1", StartedAt: day}); err != nil {
		t.Fatalf("record 1: %v", err)
	}
	if err := rec.Open("sess-double", day); err != nil {
		t.Fatalf("second open: %v", err)
	}
	if err := rec.record(&Exchange{SessionID: "sess-double", Method: "GET", URL: "http://a/2", StartedAt: day}); err != nil {
		t.Fatalf("record 2: %v", err)
	}
	if err := rec.Close("sess-double"); err != nil {
		t.Fatalf("close: %v", err)
	}

	path := filepath.Join(root, "2026-08-09", "sess-double", "exchanges.jsonl.gz")
	lines := readExchanges(t, path)
	if len(lines) != 2 {
		t.Fatalf("got %d exchange lines, want 2", len(lines))
	}
	if lines[0].Seq != 1 || lines[1].Seq != 2 {
		t.Errorf("seqs = %d, %d; want 1, 2", lines[0].Seq, lines[1].Seq)
	}
	if lines[0].URL != "http://a/1" || lines[1].URL != "http://a/2" {
		t.Errorf("urls = %q, %q; want http://a/1, http://a/2", lines[0].URL, lines[1].URL)
	}
}
