package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mrgeoffrich/deepseek-harness/internal/attachment"
)

// mcpImageExtensionByMIME resolves an MCP image content block's MIME type
// to the extension its bytes are written under, derived from
// internal/attachment.ImageMIMETypes rather than a second hand-maintained
// copy of it (docs/MCP.md, "Calling": "the extension resolved from the MIME
// type through the table in internal/attachment"). Built once, at package
// init, from a fixed preference order rather than a map iteration — image/
// jpeg has two extensions in that table (.jpg and .jpeg), and the earlier
// one in the list below wins deterministically instead of depending on
// map-iteration order.
var mcpImageExtensionByMIME = func() map[string]string {
	m := make(map[string]string, len(attachment.ImageMIMETypes))
	for _, ext := range []string{".png", ".jpg", ".jpeg", ".webp"} {
		mime, ok := attachment.ImageMIMETypes[ext]
		if !ok {
			continue
		}
		if _, exists := m[mime]; !exists {
			m[mime] = ext
		}
	}
	return m
}()

// execMCP dispatches a call whose name carries the mcp__ prefix to the
// configured provider and flattens its answer into a Result
// (docs/MCP.md, "Calling"). server is the name MCPServerOf already parsed
// out of name, so the error paths below can name it without re-parsing.
func (e *Executor) execMCP(ctx context.Context, server, name string, argsRaw json.RawMessage) Result {
	if e.MCP == nil {
		return errorResult("MCP is not configured in this context: %s cannot be called", name)
	}

	content, err := e.MCP.Call(ctx, name, argsRaw)
	if err != nil {
		return errorResult("MCP server %q: %v", server, err)
	}
	return e.placeMCPContent(ctx, server, strings.TrimPrefix(name, MCPToolPrefix+server+"__"), content)
}

// placeMCPContent turns one server's raw reply into a Result: text under
// the output cap, images written into the session's scratch/mcp/ and named
// in the text, a server-reported isError carried through as an ordinary
// failed result. Shared by the mcp__<server>__<tool> path above and by the
// fixed MCPReadResource and MCPGetPrompt tools (mcpresources.go), which
// receive exactly the same kind of content and have no reason to place it
// differently.
//
// toolPart names the half of the filename that says where an image came
// from — a tool's own name for a tool call, "resource" or "prompt" for the
// other two.
func (e *Executor) placeMCPContent(ctx context.Context, server, toolPart string, content MCPContent) Result {
	text, truncated := truncate(content.Text, e.outputCap(ctx))
	var b strings.Builder
	b.WriteString(text)

	var imageURL string
	for i, img := range content.Images {
		rel, writeErr := e.writeMCPImage(server, toolPart, img)
		if writeErr != nil {
			fmt.Fprintf(&b, "\n\n[image %d: %v]", i+1, writeErr)
			continue
		}
		fmt.Fprintf(&b, "\n\nWrote %s", rel)
		if imageURL == "" && e.SeeImages {
			imageURL = "data:" + img.MIMEType + ";base64," + base64.StdEncoding.EncodeToString(img.Data)
		}
	}

	res := Result{Content: b.String(), IsError: content.IsError, Truncated: truncated}
	// ImageURL never rides with IsError: a failed call is a refusal with a
	// text result, not a broken image part (the same rule Read follows).
	if !res.IsError {
		res.ImageURL = imageURL
	}
	return res
}

// writeMCPImage writes one MCP image content block into the workspace at
// scratch/mcp/<server>-<tool>-<n>.<ext> — the same workspace-confined
// resolution the Screenshot tool's own output uses
// (resolveScratchRelative, which resolveScratchImageOutput calls in turn),
// so an MCP image can never land outside scratch/ regardless of how server
// or toolPart are spelled. Returns the workspace-relative path so the
// result text can name it the way formatScreenshotReport names a capture,
// letting Glance be pointed at it directly. An unknown MIME type is
// reported rather than written under a guessed extension.
func (e *Executor) writeMCPImage(server, toolPart string, img MCPImage) (string, error) {
	ext, ok := mcpImageExtensionByMIME[img.MIMEType]
	if !ok {
		return "", fmt.Errorf("unknown MIME type %q, not written to a file", img.MIMEType)
	}
	rel := filepath.Join("mcp", fmt.Sprintf("%s-%s-%d%s", server, toolPart, e.nextMCPImageNumber(), ext))
	path, err := resolveScratchRelative(e.Workspace, rel)
	if err != nil {
		return "", fmt.Errorf("resolve output path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create output directory: %w", err)
	}
	if err := os.WriteFile(path, img.Data, 0o644); err != nil {
		return "", fmt.Errorf("write image: %w", err)
	}
	return filepath.Join(scratchDir, rel), nil
}

// nextMCPImageNumber returns the next per-session image number, so two
// calls to one tool never write to the same path (see Executor.mcpImageMu).
func (e *Executor) nextMCPImageNumber() int {
	e.mcpImageMu.Lock()
	defer e.mcpImageMu.Unlock()
	e.nextMCPImage++
	return e.nextMCPImage
}
