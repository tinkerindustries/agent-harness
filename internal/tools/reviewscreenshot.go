package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/mrgeoffrich/deepseek-harness/internal/gemini"
	"github.com/mrgeoffrich/deepseek-harness/internal/pricing"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

type reviewScreenshotArgs struct {
	ImagePaths []string `json:"image_paths"`
	Question   string   `json:"question"`
	Spec       string   `json:"spec"`
}

// Limits the ReviewScreenshot tool enforces on its input (docs/TOOLS.md,
// "ReviewScreenshot").
const (
	reviewScreenshotMaxImages = 4
	reviewScreenshotMaxBytes  = 5 << 20 // 5 MB per file
)

// reviewScreenshotSystemInstruction is the fixed system instruction from the
// prompt skeleton in docs/gemini-3.5-flash-ui-review-prompting.md: the
// consistency rules that replace temperature/top_p/top_k (which must not be
// set for Gemini 3.x), and the JSON element/issue/expected/actual shape the
// answer must take.
const reviewScreenshotSystemInstruction = `You are reviewing a web page screenshot against its intended design.
Be precise and concise — flag concrete issues (element name, expected vs actual position/size/color/spacing), not general impressions.
Output as a JSON list: [{ "element": "", "issue": "", "expected": "", "actual": "" }]`

// execReviewScreenshot implements ReviewScreenshot: send one to four
// screenshots to Google Gemini's vision model and return its diagnosis of
// the question. The first image goes at high resolution and the rest at
// medium, per the doc's advice that only the image needing scrutiny should
// be high — which is why the tool description tells the model to put the
// screenshot it cares about first. A nil Gemini client is an ordinary error
// result, not a panic and not a failed run: the same shape WebFetch uses for
// a nil e.Client.
func execReviewScreenshot(ctx context.Context, e *Executor, argsRaw json.RawMessage) Result {
	var args reviewScreenshotArgs
	if err := json.Unmarshal(argsRaw, &args); err != nil {
		return errorResult("invalid arguments: %v", err)
	}
	if len(args.ImagePaths) == 0 {
		return errorResult("image_paths is required and must name at least one file")
	}
	if args.Question == "" {
		return errorResult("question is required")
	}
	if len(args.ImagePaths) > reviewScreenshotMaxImages {
		return errorResult("ReviewScreenshot accepts at most %d images, got %d", reviewScreenshotMaxImages, len(args.ImagePaths))
	}

	images := make([]gemini.Image, 0, len(args.ImagePaths))
	for i, userPath := range args.ImagePaths {
		path, err := ResolvePath(e.Workspace, userPath)
		if err != nil {
			return errorResult("%v", err)
		}
		info, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				return errorResult("file not found: %s", userPath)
			}
			return errorResult("stat %s: %v", userPath, err)
		}
		if info.IsDir() {
			return errorResult("%s is a directory, not a screenshot", userPath)
		}
		if info.Size() > reviewScreenshotMaxBytes {
			return errorResult("screenshot %s is %d bytes, over the 5 MB per-file limit", userPath, info.Size())
		}
		mimeType, ok := screenshotMIMEType(path)
		if !ok {
			return errorResult("unsupported screenshot type for %s: ReviewScreenshot accepts PNG, JPEG, and WebP files", userPath)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return errorResult("read %s: %v", userPath, err)
		}

		resolution := gemini.ResolutionMedium
		if i == 0 {
			resolution = gemini.ResolutionHigh
		}
		images = append(images, gemini.Image{Data: data, MIMEType: mimeType, Resolution: resolution})
	}

	// The capability check comes after argument validation, so a call with a
	// bad extension or an oversize file learns that even when this session
	// has no Gemini client — the specific error is the one the model can
	// route around.
	if e.Gemini == nil {
		return errorResult("ReviewScreenshot is not available in this context: no Gemini client configured")
	}

	model := gemini.DefaultModel
	if e.GeminiModel != nil {
		if m, err := e.GeminiModel(); err != nil {
			return errorResult("resolve vision model: %v", err)
		} else if m != "" {
			model = m
		}
	}

	question := args.Question
	if args.Spec != "" {
		// The spec is data, so it precedes the question ("data first,
		// question last", docs/gemini-3.5-flash-ui-review-prompting.md).
		question = "Design spec / target CSS:\n" + args.Spec + "\n\n" + question
	}

	answer, usage, err := e.Gemini.Interact(ctx, model, reviewScreenshotSystemInstruction, question, images)
	if err != nil {
		return errorResult("%v", err)
	}
	out, truncated := truncate(answer, e.outputCap())
	res := Result{Content: out, Truncated: truncated}
	if usage != nil {
		res.GeminiUsage = geminiUsagePayload(e.Prices, model, usage)
	}
	return res
}

// geminiUsagePayload turns one successful Gemini call's usage into the
// store's usage-event shape, cost included, so the runner can commit it and
// the session's cost total picks it up like any DeepSeek turn. The token
// mapping (thinking billed at the output rate, cached input a subset of
// input) is decided and documented in gemini.Usage.TokenSplit. Cost is
// computed against the same price table DeepSeek's turns use, keyed by the
// vision model that actually ran. A nil price table, or a model with no
// entry, leaves cost at zero rather than failing the tool result: the run
// itself succeeded, and the operator's price table is the thing that is
// incomplete (docs/DESIGN.md §4.9 prices load from config, never from
// code).
func geminiUsagePayload(prices *pricing.Table, model string, usage *gemini.Usage) *store.UsagePayload {
	cacheHit, cacheMiss, completion, reasoning := usage.TokenSplit()
	payload := &store.UsagePayload{
		PromptTokens:          usage.TotalInputTokens,
		PromptCacheHitTokens:  cacheHit,
		PromptCacheMissTokens: cacheMiss,
		CompletionTokens:      completion,
		ReasoningTokens:       reasoning,
	}
	if prices != nil {
		if c, err := prices.Cost(model, cacheHit, cacheMiss, completion); err == nil {
			payload.CostUSD = c
		}
	}
	return payload
}

// screenshotMIMEType reports the Gemini MIME type for a screenshot file, by
// extension, and whether the extension is one of the accepted ones.
func screenshotMIMEType(path string) (string, bool) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png", true
	case ".jpg", ".jpeg":
		return "image/jpeg", true
	case ".webp":
		return "image/webp", true
	default:
		return "", false
	}
}
