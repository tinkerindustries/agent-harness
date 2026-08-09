package deepseek

import (
	"bufio"
	"io"
	"strings"
)

type frameKind int

const (
	frameBlank frameKind = iota
	frameComment
	frameData
	frameOther
)

// sseFrame is one classified SSE line.
type sseFrame struct {
	kind frameKind
	data string
}

// classifySSELine categorises one line of an SSE stream. Lines starting
// with ":" are comments; DeepSeek uses them as keep-alives while a request
// waits in queue and they carry no payload, but they still count as
// activity for an idle watchdog. data carries the JSON payload with the
// "data:" prefix stripped.
func classifySSELine(line string) sseFrame {
	switch {
	case line == "":
		return sseFrame{kind: frameBlank}
	case strings.HasPrefix(line, ":"):
		return sseFrame{kind: frameComment}
	case strings.HasPrefix(line, "data:"):
		return sseFrame{kind: frameData, data: strings.TrimSpace(strings.TrimPrefix(line, "data:"))}
	default:
		return sseFrame{kind: frameOther}
	}
}

// sseScanner reads SSE lines from a reader. bufio.Reader.ReadString grows
// its internal buffer across reads to find the delimiter, unlike
// bufio.Scanner's fixed token limit, so a single line over 64KB is read
// correctly here without any special sizing.
type sseScanner struct {
	r *bufio.Reader
}

func newSSEScanner(r io.Reader) *sseScanner {
	return &sseScanner{r: bufio.NewReader(r)}
}

// Scan returns the next line with its trailing newline stripped. It
// returns io.EOF once the underlying reader is exhausted, including after
// returning a final line that had no trailing newline.
func (s *sseScanner) Scan() (string, error) {
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
