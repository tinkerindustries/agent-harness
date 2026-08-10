package tools

import (
	"bytes"
	"io"
	"os"
	"unicode/utf8"
)

// binaryProbeSize is how many leading bytes of a file are inspected to tell
// text from binary. Anything past the probe is not read.
const binaryProbeSize = 8192

// isBinaryFile reports whether path holds binary data and the file's size in
// bytes. A file counts as binary when its first binaryProbeSize bytes contain
// a NUL byte or are not valid UTF-8; an empty file is text. The probe can cut
// a multi-byte rune in half, so any trailing partial rune is dropped before
// the UTF-8 test rather than misreporting a text file as binary. The guard
// opens the file itself and reads only the probe; callers re-read from the
// start for the real work.
func isBinaryFile(path string) (size int64, binary bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return 0, false, err
	}

	head := make([]byte, binaryProbeSize)
	n, err := io.ReadFull(f, head)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return 0, false, err
	}
	head = head[:n]
	if len(head) == 0 {
		return info.Size(), false, nil
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return info.Size(), true, nil
	}
	head = trimTruncatedRune(head, n)
	return info.Size(), !utf8.Valid(head), nil
}

// trimTruncatedRune drops a trailing rune that the probe cut in half, so a
// text file is not reported as binary for stopping mid-character. Only a
// probe that filled its buffer can have cut anything, and only bytes that
// could belong to an incomplete rune are dropped: trimming a byte that can
// never appear in a valid encoding, such as 0xFF, would report binary data
// as text.
func trimTruncatedRune(head []byte, n int) []byte {
	if n < binaryProbeSize {
		return head
	}
	for trimmed := 0; trimmed < utf8.UTFMax-1 && len(head) > 0; {
		c := head[len(head)-1]
		if utf8.RuneStart(c) {
			// A lead byte announcing more bytes than the probe kept is the
			// cut; anything else is data in its own right.
			if leadRuneLen(c) > trimmed+1 {
				head = head[:len(head)-1]
			}
			break
		}
		head = head[:len(head)-1]
		trimmed++
	}
	return head
}

// leadRuneLen returns how many bytes the rune starting with c occupies, or 0
// when c cannot start one.
func leadRuneLen(c byte) int {
	switch {
	case c >= 0xC2 && c <= 0xDF:
		return 2
	case c >= 0xE0 && c <= 0xEF:
		return 3
	case c >= 0xF0 && c <= 0xF4:
		return 4
	}
	return 0
}

// binaryFileError is the result both Read and Edit return when the guard
// fires, so the model learns why it got nothing instead of guessing.
func binaryFileError(path string, size int64) Result {
	return errorResult("cannot read a binary file: %s (%d bytes)", path, size)
}
