package wire

import (
	"io"
	"strings"
	"testing"
)

func TestSSEScannerBasicLines(t *testing.T) {
	input := "data: {\"a\":1}\n\ndata: {\"a\":2}\n"
	s := NewSSEScanner(strings.NewReader(input))

	want := []string{`data: {"a":1}`, "", `data: {"a":2}`}
	for i, w := range want {
		line, err := s.Scan()
		if err != nil {
			t.Fatalf("Scan() #%d: unexpected error: %v", i, err)
		}
		if line != w {
			t.Errorf("Scan() #%d = %q, want %q", i, line, w)
		}
	}
	if _, err := s.Scan(); err != io.EOF {
		t.Errorf("final Scan() error = %v, want io.EOF", err)
	}
}

func TestSSEScannerLineOverScannerLimit(t *testing.T) {
	// bufio.Scanner's default token limit is 64KB. A single SSE data line
	// can exceed it; the scanner must return it intact regardless.
	huge := strings.Repeat("x", 200*1024)
	input := "data: " + huge + "\n"
	s := NewSSEScanner(strings.NewReader(input))

	line, err := s.Scan()
	if err != nil {
		t.Fatalf("Scan(): unexpected error: %v", err)
	}
	want := "data: " + huge
	if line != want {
		t.Fatalf("Scan() returned %d bytes, want %d bytes", len(line), len(want))
	}
}

func TestSSEScannerNoTrailingNewline(t *testing.T) {
	s := NewSSEScanner(strings.NewReader("data: [DONE]"))
	line, err := s.Scan()
	if err != nil {
		t.Fatalf("Scan(): unexpected error: %v", err)
	}
	if line != "data: [DONE]" {
		t.Errorf("Scan() = %q, want %q", line, "data: [DONE]")
	}
	if _, err := s.Scan(); err != io.EOF {
		t.Errorf("second Scan() error = %v, want io.EOF", err)
	}
}

func TestSSEScannerEmptyInput(t *testing.T) {
	s := NewSSEScanner(strings.NewReader(""))
	if _, err := s.Scan(); err != io.EOF {
		t.Errorf("Scan() of empty input error = %v, want io.EOF", err)
	}
}

func TestClassifySSELine(t *testing.T) {
	cases := []struct {
		line     string
		wantKind FrameKind
		wantData string
	}{
		{"", FrameBlank, ""},
		{": keep-alive", FrameComment, ""},
		{":", FrameComment, ""},
		{"data: {\"a\":1}", FrameData, `{"a":1}`},
		{"data: [DONE]", FrameData, "[DONE]"},
		{"data:{\"a\":1}", FrameData, `{"a":1}`},
		{"event: message", FrameOther, ""},
	}
	for _, c := range cases {
		f := ClassifySSELine(c.line)
		if f.Kind != c.wantKind {
			t.Errorf("ClassifySSELine(%q).Kind = %v, want %v", c.line, f.Kind, c.wantKind)
		}
		if f.Data != c.wantData {
			t.Errorf("ClassifySSELine(%q).Data = %q, want %q", c.line, f.Data, c.wantData)
		}
	}
}
