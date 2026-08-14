package tools

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/gemini/geminitest"
)

// askVisionExecutor returns an executor whose Gemini client points at srv,
// with one decodable PNG on disk.
func askVisionExecutor(t *testing.T, srv *httptest.Server) *Executor {
	t.Helper()
	e, err := NewExecutor(t.TempDir(), &Policy{Mode: ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	e.Gemini = reviewScreenshotClient(srv)
	if err := os.WriteFile(filepath.Join(e.Workspace, "shot.png"), testPNGBytes(t, 8, 8), 0o644); err != nil {
		t.Fatal(err)
	}
	return e
}

// TestAskVisionReturnsAnswerVerbatim is the point of the tool: whatever the
// vision model says comes back as text, with nothing imposed on its shape.
// The prose here is deliberately not JSON and not a findings list — through
// ReviewScreenshot the same answer would be relabelled as unparsed text, and
// through this tool it is simply the answer.
func TestAskVisionReturnsAnswerVerbatim(t *testing.T) {
	const answer = "The left image shows a login form; the right shows the same form with the submit button missing."
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, geminitest.Answer(answer, ""))
	}))
	defer srv.Close()

	e := askVisionExecutor(t, srv)
	res := runTool(t, e, "AskVision", askVisionArgs{
		ImagePaths: []string{"shot.png"},
		Prompt:     "Compare these two captures. What is different?",
	})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, answer) {
		t.Fatalf("answer not returned verbatim.\ngot:  %s\nwant it to contain: %s", res.Content, answer)
	}
	if strings.Contains(res.Content, "finding") || strings.Contains(res.Content, "not in the expected JSON shape") {
		t.Fatalf("prose answer was reshaped as findings: %s", res.Content)
	}
}

// TestAskVisionSendsTheCallersPromptUnchanged pins the other half of the
// bargain: the prompt reaches Gemini as written. ReviewScreenshot prepends a
// spec and rewrites a follow-up; this tool must not, because the caller is now
// the one responsible for the prompt being any good.
func TestAskVisionSendsTheCallersPromptUnchanged(t *testing.T) {
	const prompt = "Target: the badge sits left of the title on one line.\n\nBased on the preceding, is that what the capture shows?"
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, geminitest.Answer("A badge and a title on one line.", ""))
	}))
	defer srv.Close()

	e := askVisionExecutor(t, srv)
	res := runTool(t, e, "AskVision", askVisionArgs{
		ImagePaths: []string{"shot.png"},
		Prompt:     prompt,
	})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(string(body), "Based on the preceding, is that what the capture shows?") {
		t.Fatalf("prompt did not reach the request body verbatim: %s", body)
	}
	// The one thing the tool does impose, and the reason it imposes it: an
	// answer with no statement of what was seen cannot be checked by a reader
	// who cannot open the image.
	if !strings.Contains(string(body), "what is actually visible") {
		t.Fatalf("system instruction did not carry the observed-first rule: %s", body)
	}
}

func TestAskVisionValidatesArguments(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request should be sent for invalid arguments")
	}))
	defer srv.Close()
	e := askVisionExecutor(t, srv)

	for _, tc := range []struct {
		name string
		args askVisionArgs
		want string
	}{
		{"no images", askVisionArgs{Prompt: "what is this?"}, "image_paths is required"},
		{"no prompt", askVisionArgs{ImagePaths: []string{"shot.png"}}, "prompt is required"},
		{"bad thinking level", askVisionArgs{ImagePaths: []string{"shot.png"}, Prompt: "?", ThinkingLevel: "ultra"}, "thinking_level must be low, medium, or high"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := runTool(t, e, "AskVision", tc.args)
			if !res.IsError || !strings.Contains(res.Content, tc.want) {
				t.Fatalf("want an error containing %q, got: %s", tc.want, res.Content)
			}
		})
	}
}

// TestAskVisionThinkingLevelDefaults pins the default and the accepted set.
// minimal is deliberately not offered: the doc's own table puts it at
// chat-like factual answers, below the quality this use case needs.
func TestAskVisionThinkingLevelDefaults(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", "medium"},
		{"low", "low"},
		{"MEDIUM", "medium"},
		{" high ", "high"},
	} {
		got, err := askVisionThinkingLevel(tc.in)
		if err != nil {
			t.Fatalf("askVisionThinkingLevel(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("askVisionThinkingLevel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if _, err := askVisionThinkingLevel("minimal"); err == nil {
		t.Error("minimal should be refused: it is below the quality this use case needs")
	}
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
