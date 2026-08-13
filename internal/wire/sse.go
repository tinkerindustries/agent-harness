package wire

import (
	"bufio"
	"io"
	"strings"
)

// FrameKind classifies one line of an SSE stream.
type FrameKind int

const (
	FrameBlank FrameKind = iota
	FrameComment
	FrameData
	FrameOther
)

// SSEFrame is one classified SSE line.
type SSEFrame struct {
	Kind FrameKind
	Data string
}

// ClassifySSELine categorises one line of an SSE stream. Lines starting
// with ":" are comments; the providers use them as keep-alives while a
// request waits in queue and they carry no payload, but they still count as
// activity for an idle watchdog. Data carries the JSON payload with the
// "data:" prefix stripped.
func ClassifySSELine(line string) SSEFrame {
	switch {
	case line == "":
		return SSEFrame{Kind: FrameBlank}
	case strings.HasPrefix(line, ":"):
		return SSEFrame{Kind: FrameComment}
	case strings.HasPrefix(line, "data:"):
		return SSEFrame{Kind: FrameData, Data: strings.TrimSpace(strings.TrimPrefix(line, "data:"))}
	default:
		return SSEFrame{Kind: FrameOther}
	}
}

// SSEScanner reads SSE lines from a reader. bufio.Reader.ReadString grows
// its internal buffer across reads to find the delimiter, unlike
// bufio.Scanner's fixed token limit, so a single line over 64KB is read
// correctly here without any special sizing.
type SSEScanner struct {
	r *bufio.Reader
}

// NewSSEScanner returns a scanner reading from r.
func NewSSEScanner(r io.Reader) *SSEScanner {
	return &SSEScanner{r: bufio.NewReader(r)}
}

// Scan returns the next line with its trailing newline stripped. It
// returns io.EOF once the underlying reader is exhausted, including after
// returning a final line that had no trailing newline.
func (s *SSEScanner) Scan() (string, error) {
	line, err := s.r.ReadString('\n')
	line = strings.TrimRight(line, "\r\n")
	if err != nil {
		if err == io.EOF {
			if line == "" {
				return "", io.EOF
			}
			return line, nil
		}
		return "", err
	}
	return line, nil
}
