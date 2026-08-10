package tools

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsBinaryFileRejectsNULByte(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bin.dat")
	content := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x00}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}

	size, binary, err := isBinaryFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !binary {
		t.Fatal("expected a file with a NUL byte to be binary")
	}
	if size != int64(len(content)) {
		t.Fatalf("size = %d, want %d", size, len(content))
	}
}

func TestIsBinaryFileRejectsInvalidUTF8(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.txt")
	if err := os.WriteFile(path, []byte("text\xff\xfe tail"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, binary, err := isBinaryFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !binary {
		t.Fatal("expected invalid UTF-8 to be binary")
	}
}

func TestIsBinaryFileAcceptsText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "good.txt")
	content := "plain text\nwith unicode: 你好世界\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	_, binary, err := isBinaryFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if binary {
		t.Fatal("expected valid UTF-8 to be text")
	}
}

func TestIsBinaryFileAcceptsEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.txt")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	size, binary, err := isBinaryFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if binary || size != 0 {
		t.Fatalf("expected an empty file to be text of size 0, got binary=%v size=%d", binary, size)
	}
}

// The probe cuts at 8192 bytes, which can split a multi-byte rune in half;
// that split tail must not turn a text file into a binary one.
func TestIsBinaryFileAcceptsRuneSplitAtProbeBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "split.txt")
	content := strings.Repeat("a", binaryProbeSize-2) + "你" // 8190 + 3 bytes
	if len(content) != binaryProbeSize+1 {
		t.Fatalf("test setup: len = %d, want %d", len(content), binaryProbeSize+1)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	_, binary, err := isBinaryFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if binary {
		t.Fatal("expected a text file with a rune split at the probe boundary to stay text")
	}
}

// Invalid UTF-8 past the probe is not the tool's problem: the probe region
// decides, so a text head with a binary tail is reported as text.
func TestIsBinaryFileIgnoresBytesPastProbe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tail.dat")
	content := make([]byte, binaryProbeSize+8)
	for i := range content {
		content[i] = 'a'
	}
	copy(content[binaryProbeSize:], "\xff\xfe")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}

	_, binary, err := isBinaryFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if binary {
		t.Fatal("expected bytes past the probe not to count")
	}
}

func TestReadRejectsBinaryFile(t *testing.T) {
	e, root := newTestExecutor(t)
	content := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 'a', 'b', 'c'}
	if err := os.WriteFile(filepath.Join(root, "image.png"), content, 0o644); err != nil {
		t.Fatal(err)
	}

	res := execRead(t.Context(), e, mustJSON(t, readArgs{FilePath: "image.png"}))
	if !res.IsError {
		t.Fatal("expected an error reading a binary file")
	}
	if !strings.Contains(res.Content, "cannot read a binary file: image.png (11 bytes)") {
		t.Fatalf("expected a binary-file error naming the file and size, got: %s", res.Content)
	}
}

func TestReadAcceptsEmptyFile(t *testing.T) {
	e, root := newTestExecutor(t)
	writeFile(t, root, "empty.txt", "")

	res := execRead(t.Context(), e, mustJSON(t, readArgs{FilePath: "empty.txt"}))
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "(file is empty)") {
		t.Fatalf("expected the empty-file message, got: %s", res.Content)
	}
}

func TestEditRejectsBinaryFileWithoutModifyingIt(t *testing.T) {
	e, root := newTestExecutor(t)
	content := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 'a', 'b', 'c'}
	path := filepath.Join(root, "image.png")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}

	res := execEdit(t.Context(), e, editArgsJSON(t, editArgs{
		FilePath: "image.png", OldString: "abc", NewString: "xyz",
	}))
	if !res.IsError {
		t.Fatal("expected an error editing a binary file")
	}
	if !strings.Contains(res.Content, "cannot read a binary file: image.png (11 bytes)") {
		t.Fatalf("expected a binary-file error naming the file and size, got: %s", res.Content)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("Edit modified a binary file it refused: %q", got)
	}
}
