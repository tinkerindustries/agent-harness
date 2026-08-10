package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/mrgeoffrich/deepseek-harness/internal/deepseek"
	"github.com/mrgeoffrich/deepseek-harness/internal/settings"
)

type webFetchArgs struct {
	URL    string `json:"url"`
	Prompt string `json:"prompt"`
}

// webFetchMaxBody bounds how much of a fetched page is read before
// extraction; webFetchMaxExtract bounds how much extracted text is sent to
// the summarising model, so one huge page cannot blow the budget of the
// flash call that reads it (docs/MODELS.md: WebFetch extraction gets 4000
// max_tokens of output). Production resolves both through the settings
// registry (tools.webfetch_max_body, tools.webfetch_max_extract); these are
// the built-in defaults, pinned equal by internal/settings/registry_test.go.
const (
	webFetchMaxBody    = 4 << 20
	webFetchMaxExtract = 40_000
)

func (e *Executor) webFetchMaxBody(ctx context.Context) int {
	if e.Settings != nil {
		if v, err := e.Settings.Int(ctx, settings.KeyToolWebFetchMaxBody); err == nil {
			return v
		}
	}
	return webFetchMaxBody
}

func (e *Executor) webFetchMaxExtract(ctx context.Context) int {
	if e.Settings != nil {
		if v, err := e.Settings.Int(ctx, settings.KeyToolWebFetchMaxExtract); err == nil {
			return v
		}
	}
	return webFetchMaxExtract
}

// execWebFetch implements WebFetch: fetch a URL, extract it to text, then
// have flash answer the caller's prompt against that text, so the parent
// context receives an answer rather than a page (docs/TOOLS.md).
func execWebFetch(ctx context.Context, e *Executor, argsRaw json.RawMessage) Result {
	var args webFetchArgs
	if err := json.Unmarshal(argsRaw, &args); err != nil {
		return errorResult("invalid arguments: %v", err)
	}
	if args.URL == "" {
		return errorResult("url is required")
	}
	if args.Prompt == "" {
		return errorResult("prompt is required")
	}
	u, err := url.Parse(args.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return errorResult("url must be http or https: %q", args.URL)
	}
	if e.Client == nil {
		return errorResult("WebFetch is not available in this context: no client configured")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, args.URL, nil)
	if err != nil {
		return errorResult("build request: %v", err)
	}
	req.Header.Set("User-Agent", "deepseek-harness/1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return errorResult("fetch %s: %v", args.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errorResult("fetch %s: HTTP %d", args.URL, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(e.webFetchMaxBody(ctx))))
	if err != nil {
		return errorResult("read %s: %v", args.URL, err)
	}

	extracted := extractText(string(body))
	if extracted == "" {
		return Result{Content: "fetched the page but found no extractable text"}
	}
	if len(extracted) > e.webFetchMaxExtract(ctx) {
		extracted = extracted[:e.webFetchMaxExtract(ctx)]
	}

	answer, err := e.summarizeFetch(ctx, extracted, args.Prompt)
	if err != nil {
		return errorResult("summarise %s: %v", args.URL, err)
	}
	out, truncated := truncate(answer, e.outputCap(ctx))
	return Result{Content: out, Truncated: truncated}
}

// summarizeFetch asks flash, thinking disabled, to answer prompt against
// content. Non-thinking side work like this can force output shape and
// runs cheaper than the main loop (docs/MODELS.md).
func (e *Executor) summarizeFetch(ctx context.Context, content, prompt string) (string, error) {
	model := e.FlashModel
	if model == "" {
		model = "deepseek-v4-flash"
	}
	req := deepseek.ChatCompletionRequest{
		Model: model,
		Messages: []deepseek.Message{
			deepseek.SystemMessage("Answer the question using only the page content the user provides. If the answer is not present in it, say so plainly."),
			deepseek.UserMessage(fmt.Sprintf("Question: %s\n\nPage content:\n%s", prompt, content)),
		},
		Thinking:  &deepseek.ThinkingConfig{Type: deepseek.ThinkingDisabled},
		MaxTokens: 4000,
	}
	resp, err := e.Client.CreateChatCompletion(ctx, req)
	if err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("no choices returned")
	}
	return resp.Choices[0].Message.Content, nil
}

var (
	scriptStyleTags = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)
	htmlTags        = regexp.MustCompile(`(?s)<[^>]*>`)
	repeatedSpace   = regexp.MustCompile(`[ \t]+`)
	repeatedBlank   = regexp.MustCompile(`\n{3,}`)
)

// extractText strips scripts, styles, and tags from HTML and collapses
// whitespace, without pulling in an HTML parsing dependency.
func extractText(body string) string {
	s := scriptStyleTags.ReplaceAllString(body, " ")
	s = htmlTags.ReplaceAllString(s, "\n")
	s = html.UnescapeString(s)
	s = repeatedSpace.ReplaceAllString(s, " ")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	s = strings.Join(lines, "\n")
	s = repeatedBlank.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}
