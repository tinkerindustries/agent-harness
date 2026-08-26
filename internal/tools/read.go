package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/mrgeoffrich/agent-harness/internal/attachment"
)

type readArgs struct {
	FilePath string `json:"file_path"`
	Offset   int    `json:"offset"`
	Limit    int    `json:"limit"`
}

// execRead implements Read: cat -n style output, because that is the shape
// the target harnesses return and the model reads offsets out of it
// (docs/TOOLS.md).
func execRead(ctx context.Context, e *Executor, argsRaw json.RawMessage) Result {
	var args readArgs
	if err := json.Unmarshal(argsRaw, &args); err != nil {
		return errorResult("invalid arguments: %v", err)
	}
	if args.FilePath == "" {
		return errorResult("file_path is required")
	}
	path, err := ResolvePath(e.Workspace, args.FilePath)
	if err != nil {
		return errorResult("%v", err)
	}

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return errorResult("file not found: %s", args.FilePath)
		}
		return errorResult("open %s: %v", args.FilePath, err)
	}
	defer f.Close()

	if info, err := f.Stat(); err == nil && info.IsDir() {
		return errorResult("%s is a directory, not a file", args.FilePath)
	}

	size, binary, err := isBinaryFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return errorResult("file not found: %s", args.FilePath)
		}
		return errorResult("open %s: %v", args.FilePath, err)
	}
	if binary {
		if !e.SeeImages {
			// DeepSeek is text-only: unchanged behaviour, byte for byte. An
			// image is a binary file to it, refused like any other.
			// (docs/KIMI-INTEGRATION.md §4.5.)
			return binaryFileError(args.FilePath, size)
		}
		if mime, ok := attachment.MIMEType(path); ok {
			return e.readImage(ctx, path, args.FilePath, mime)
		}
		// A binary file that is not one of the three image formats. SVG
		// never reaches this branch — it is XML, valid UTF-8 with no NUL
		// byte, so it reads as text and the model sees the source, which is
		// what the API itself prescribes for SVG
		// (third_party/kimi-docs/guide/use-kimi-vision-model.md).
		return errorResult("cannot read a binary file: %s (%d bytes). Only PNG, JPEG, and WebP are returned as image parts; an SVG reads as text (it is XML source); any other binary type is refused", args.FilePath, size)
	}

	start := args.Offset
	if start < 1 {
		start = 1
	}
	limit := args.Limit
	if limit <= 0 {
		limit = 2000
	}

	var b strings.Builder
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	line := 0
	written := 0
	for scanner.Scan() {
		line++
		if line < start {
			continue
		}
		if written >= limit {
			break
		}
		fmt.Fprintf(&b, "%6d\t%s\n", line, scanner.Text())
		written++
	}
	if err := scanner.Err(); err != nil {
		return errorResult("read %s: %v", args.FilePath, err)
	}
	if written == 0 {
		if line == 0 {
			return Result{Content: "(file is empty)"}
		}
		return errorResult("offset %d is past the end of the file (%d lines)", start, line)
	}

	e.markRead(path)
	out, truncated := truncate(b.String(), e.outputCap(ctx))
	return Result{Content: out, Truncated: truncated}
}

// readImage returns asPath as an image_url part for a vision provider: the
// file's bytes base64-encoded into a data URI of the exact shape the Kimi
// guide prescribes — data:image/<fmt>;base64,... — with a short text label
// alongside, so the model and the transcript both know which file the image
// is (third_party/kimi-docs/guide/use-kimi-vision-model.md). The runner
// stores Content and ImageURL on the tool_result event and the fold rebuilds
// the parts array from them (internal/fold/fold.go).
//
// A refusal here is always a tool result the model can act on, never a
// failed run. Two bounds are enforced:
//
//   - The file must actually be an image of the format its extension names.
//     A binary file misnamed .png would otherwise enter the conversation and
//     be re-sent on every sub-turn of the frozen prefix before the API
//     rejected it — the most expensive way to discover the mistake.
//   - The encoded size is capped by the existing vision cap setting
//     (tools.reviewscreenshot_max_bytes, the same default of 5 MB Glance,
//     Ground, and Detect enforce per file — docs/CACHE.md: the tool
//     description names no numbers, and the model discovers the actual limit
//     from this refusal). Images sit in the frozen-prefix conversation and
//     are re-sent every sub-turn, so an unbounded image would bloat every
//     request after it; over the cap, the refusal names the limit and
//     suggests resizing.
func (e *Executor) readImage(ctx context.Context, resolved, asPath, mime string) Result {
	limit := e.visionMaxBytes(ctx)
	// A file already over the cap cannot fit encoded (the data URI is
	// strictly larger than the raw bytes), so refuse before reading it into
	// memory — an over-cap image is the one case the size check exists for.
	if info, err := os.Stat(resolved); err == nil && info.Size() > int64(limit) {
		return errorResult("cannot read %s: the image is %d bytes, over the %d-byte cap for images (the encoded data URI would be larger still). Resize or re-encode the image (a viewport-sized PNG or JPEG is usually small enough) and retry", asPath, info.Size(), limit)
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return errorResult("read %s: %v", asPath, err)
	}
	if !imageSignatureMatches(data, mime) {
		return errorResult("cannot read %s: the file is not a %s image — its bytes do not match the format its extension names", asPath, mime)
	}
	uri := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
	if len(uri) > limit {
		return errorResult("cannot read %s: the encoded image is %d bytes, over the %d-byte cap for images. Resize or re-encode the image (a viewport-sized PNG or JPEG is usually small enough) and retry", asPath, len(uri), limit)
	}
	return Result{Content: "Image: " + asPath, ImageURL: uri}
}

// imageSignatureMatches reports whether data starts with the magic bytes of
// the format mime names. The check is deliberately by signature, not by
// extension: a file that will not decode as its extension's format must not
// enter the conversation (see readImage).
func imageSignatureMatches(data []byte, mime string) bool {
	switch mime {
	case "image/png":
		return len(data) >= 8 && bytes.Equal(data[:8], []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	case "image/jpeg":
		return len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF
	case "image/webp":
		return len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP"))
	}
	return false
}
