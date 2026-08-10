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
	for len(head) > 0 {
		if r, size := utf8.DecodeLastRune(head); r != utf8.RuneError || size > 1 {
			break
		}
		head = head[:len(head)-1]
	}
	return info.Size(), !utf8.Valid(head), nil
}

// binaryFileError is the result both Read and Edit return when the guard
// fires, so the model learns why it got nothing instead of guessing.
func binaryFileError(path string, size int64) Result {
	return errorResult("cannot read a binary file: %s (%d bytes)", path, size)
}
