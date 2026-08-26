package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// fakeMCPProvider is a minimal tools.MCPProvider for exercising Execute's
// MCP routing and execMCP's flattening without a real internal/mcpclient
// connection. Call answers keyed by the qualified tool name Execute passes
// in, matching what a real MCPProvider.Call receives.
type fakeMCPProvider struct {
	content   map[string]MCPContent
	err       map[string]error
	resources []MCPResource
	prompts   []MCPPrompt
	// readContent answers ReadResource and GetPrompt, keyed by the server
	// and URI or prompt name the call named, joined by a space.
	readContent map[string]MCPContent
	readErr     map[string]error
}

func (f *fakeMCPProvider) Definitions(ctx context.Context) ([]wire.Tool, map[string]bool, error) {
	return nil, nil, nil
}

func (f *fakeMCPProvider) Instructions(ctx context.Context) (map[string]string, error) {
	return nil, nil
}

func (f *fakeMCPProvider) Resources(ctx context.Context) ([]MCPResource, error) {
	return f.resources, nil
}

func (f *fakeMCPProvider) Prompts(ctx context.Context) ([]MCPPrompt, error) {
	return f.prompts, nil
}

func (f *fakeMCPProvider) ReadResource(ctx context.Context, server, uri string) (MCPContent, error) {
	return f.answer(server + " " + uri)
}

func (f *fakeMCPProvider) GetPrompt(ctx context.Context, server, name string, _ map[string]string) (MCPContent, error) {
	return f.answer(server + " " + name)
}

func (f *fakeMCPProvider) answer(key string) (MCPContent, error) {
	if err, ok := f.readErr[key]; ok {
		return MCPContent{}, err
	}
	return f.readContent[key], nil
}

func (f *fakeMCPProvider) Call(ctx context.Context, toolName string, args json.RawMessage) (MCPContent, error) {
	if f.err != nil {
		if err, ok := f.err[toolName]; ok {
			return MCPContent{}, err
		}
	}
	return f.content[toolName], nil
}

func TestExecMCPReturnsText(t *testing.T) {
	e, _ := newTestExecutor(t)
	e.MCP = &fakeMCPProvider{content: map[string]MCPContent{
		"mcp__blender__get_objects_summary": {Text: "Cube, Camera, Light"},
	}}

	res := runTool(t, e, "mcp__blender__get_objects_summary", struct{}{})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if res.Content != "Cube, Camera, Light" {
		t.Fatalf("Content = %q, want the provider's text verbatim", res.Content)
	}
	if res.Truncated {
		t.Fatal("short text must not be reported as truncated")
	}
}

// TestExecMCPTruncatesAtTheOutputCap pins that an MCP result's text is
// subject to the executor's own output cap and truncation label, exactly
// like every other tool's result (docs/MCP.md, "Calling": "The output cap
// and truncation label are the harness's own").
func TestExecMCPTruncatesAtTheOutputCap(t *testing.T) {
	e, _ := newTestExecutor(t)
	e.OutputCap = 10
	e.MCP = &fakeMCPProvider{content: map[string]MCPContent{
		"mcp__blender__search_api_docs": {Text: "0123456789 and a lot more text past the cap"},
	}}

	res := runTool(t, e, "mcp__blender__search_api_docs", struct{}{})
	if !res.Truncated {
		t.Fatal("expected the result to be marked truncated")
	}
	if !strings.HasPrefix(res.Content, "0123456789") {
		t.Fatalf("Content = %q, want it to start with the kept bytes", res.Content)
	}
	if !strings.Contains(res.Content, "truncated") {
		t.Fatalf("Content = %q, want a truncation label", res.Content)
	}
}

// TestExecMCPWritesImageUnderScratch pins the image-writing contract: an
// image content block lands at scratch/mcp/<server>-<tool>-<n>.<ext>, with
// the extension resolved from the MIME type, and the result text names the
// workspace-relative path so Glance can be pointed at it. <n> counts up per
// session rather than per call, which the second half of this test is
// about: calling one tool twice must produce two files, because a run that
// renders, looks, changes something and renders again would otherwise
// overwrite the image its own earlier transcript entry points at.
func TestExecMCPWritesImageUnderScratch(t *testing.T) {
	e, root := newTestExecutor(t)
	pngBytes := []byte{0x89, 'P', 'N', 'G', 1, 2, 3, 4}
	e.MCP = &fakeMCPProvider{content: map[string]MCPContent{
		"mcp__blender__get_screenshot_of_window_as_image": {
			Text:   "captured",
			Images: []MCPImage{{MIMEType: "image/png", Data: pngBytes}},
		},
	}}

	res := runTool(t, e, "mcp__blender__get_screenshot_of_window_as_image", struct{}{})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	wantPath := filepath.Join("scratch", "mcp", "blender-get_screenshot_of_window_as_image-1.png")
	if !strings.Contains(res.Content, wantPath) {
		t.Fatalf("Content = %q, want it to name %q", res.Content, wantPath)
	}
	got, err := os.ReadFile(filepath.Join(root, wantPath))
	if err != nil {
		t.Fatalf("expected the image written at %s: %v", wantPath, err)
	}
	if string(got) != string(pngBytes) {
		t.Fatal("written bytes do not match the provider's image data")
	}

	// The same call again: a second file, not a silent overwrite of the
	// first.
	res = runTool(t, e, "mcp__blender__get_screenshot_of_window_as_image", struct{}{})
	if res.IsError {
		t.Fatalf("unexpected error on the second call: %s", res.Content)
	}
	secondPath := filepath.Join("scratch", "mcp", "blender-get_screenshot_of_window_as_image-2.png")
	if !strings.Contains(res.Content, secondPath) {
		t.Fatalf("second Content = %q, want it to name %q", res.Content, secondPath)
	}
	if _, err := os.Stat(filepath.Join(root, wantPath)); err != nil {
		t.Fatalf("the first image no longer exists after a second call: %v", err)
	}
}

// TestExecMCPFirstImageRidesBackAsImageURLOnVisionProvider pins the vision
// path: on a provider that reads images natively, the first image also
// comes back as Result.ImageURL, the same shape Read uses.
func TestExecMCPFirstImageRidesBackAsImageURLOnVisionProvider(t *testing.T) {
	e, _ := newTestExecutor(t)
	e.SeeImages = true
	pngBytes := []byte{0x89, 'P', 'N', 'G'}
	e.MCP = &fakeMCPProvider{content: map[string]MCPContent{
		"mcp__blender__get_screenshot_of_window_as_image": {
			Images: []MCPImage{{MIMEType: "image/png", Data: pngBytes}},
		},
	}}

	res := runTool(t, e, "mcp__blender__get_screenshot_of_window_as_image", struct{}{})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	wantURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngBytes)
	if res.ImageURL != wantURL {
		t.Fatalf("ImageURL = %q, want %q", res.ImageURL, wantURL)
	}
}

// TestExecMCPUnknownMIMETypeIsReportedNotWritten pins the refusal path for
// an image content block whose MIME type has no entry in
// internal/attachment.ImageMIMETypes: it is named in the text rather than
// written to a guessed extension.
func TestExecMCPUnknownMIMETypeIsReportedNotWritten(t *testing.T) {
	e, root := newTestExecutor(t)
	e.MCP = &fakeMCPProvider{content: map[string]MCPContent{
		"mcp__blender__render_thumbnail_to_path": {
			Images: []MCPImage{{MIMEType: "image/gif", Data: []byte("gif89a")}},
		},
	}}

	res := runTool(t, e, "mcp__blender__render_thumbnail_to_path", struct{}{})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "image/gif") {
		t.Fatalf("Content = %q, want it to name the unknown MIME type", res.Content)
	}
	if entries, _ := os.ReadDir(filepath.Join(root, "scratch", "mcp")); len(entries) != 0 {
		t.Fatalf("expected nothing written for an unknown MIME type, found %v", entries)
	}
}

func TestExecMCPIsErrorPropagatesAndSuppressesImageURL(t *testing.T) {
	e, _ := newTestExecutor(t)
	e.SeeImages = true
	e.MCP = &fakeMCPProvider{content: map[string]MCPContent{
		"mcp__blender__execute_blender_code": {
			Text:    "NameError: bpy is not defined",
			IsError: true,
			Images:  []MCPImage{{MIMEType: "image/png", Data: []byte{1, 2, 3}}},
		},
	}}

	res := runTool(t, e, "mcp__blender__execute_blender_code", struct{}{})
	if !res.IsError {
		t.Fatal("expected IsError to carry through from the server's isError")
	}
	if res.ImageURL != "" {
		t.Fatalf("ImageURL = %q, want empty: it must never ride with an error result", res.ImageURL)
	}
}

// TestExecMCPProviderErrorBecomesAnErrorResultNotAGoError pins the rule a
// down server must not violate: a Call error becomes an error Result naming
// the server and the reason, never a returned Go error that would fail the
// run (docs/MCP.md, "A call to a server that will not connect returns an
// error result naming the server and the reason. A run never fails because
// an MCP server is down.").
func TestExecMCPProviderErrorBecomesAnErrorResultNotAGoError(t *testing.T) {
	e, _ := newTestExecutor(t)
	e.MCP = &fakeMCPProvider{err: map[string]error{
		"mcp__blender__get_objects_summary": errors.New("dial tcp: connection refused"),
	}}

	res := runTool(t, e, "mcp__blender__get_objects_summary", struct{}{})
	if !res.IsError {
		t.Fatal("expected an error result")
	}
	for _, want := range []string{"blender", "connection refused"} {
		if !strings.Contains(res.Content, want) {
			t.Fatalf("Content = %q, want it to contain %q", res.Content, want)
		}
	}
}

// TestExecMCPNilProviderIsAnOrdinaryErrorResult pins the nil-provider case:
// no MCP client wired makes a call return an ordinary error result, never a
// panic.
func TestExecMCPNilProviderIsAnOrdinaryErrorResult(t *testing.T) {
	e, _ := newTestExecutor(t)
	// e.MCP left nil, the zero value.

	res := runTool(t, e, "mcp__blender__get_objects_summary", struct{}{})
	if !res.IsError {
		t.Fatal("expected an error result when no MCP provider is configured")
	}
	if !strings.Contains(res.Content, "mcp__blender__get_objects_summary") {
		t.Fatalf("Content = %q, want it to name the tool", res.Content)
	}
}
