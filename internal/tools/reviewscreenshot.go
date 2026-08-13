package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mrgeoffrich/deepseek-harness/internal/gemini"
	"github.com/mrgeoffrich/deepseek-harness/internal/pricing"
	"github.com/mrgeoffrich/deepseek-harness/internal/settings"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

type reviewScreenshotArgs struct {
	ImagePaths []string `json:"image_paths"`
	Question   string   `json:"question"`
	Spec       string   `json:"spec"`
}

// Limits the ReviewScreenshot tool enforces on its input (docs/TOOLS.md,
// "ReviewScreenshot"). Production resolves both through the settings
// registry (tools.reviewscreenshot_max_images, tools.reviewscreenshot_max_bytes);
// these are the built-in defaults, pinned equal by
// internal/settings/registry_test.go. The tool description names no numbers
// (docs/CACHE.md: the tool array is part of the frozen request head), so the
// model learns a changed limit from the refusal message instead.
const (
	reviewScreenshotMaxImages = 4
	reviewScreenshotMaxBytes  = 5 << 20 // 5 MB per file
)

func (e *Executor) reviewScreenshotMaxImages(ctx context.Context) int {
	if e.Settings != nil {
		if v, err := e.Settings.Int(ctx, settings.KeyToolReviewScreenshotMaxImages); err == nil {
			return v
		}
	}
	return reviewScreenshotMaxImages
}

func (e *Executor) reviewScreenshotMaxBytes(ctx context.Context) int {
	if e.Settings != nil {
		if v, err := e.Settings.Int(ctx, settings.KeyToolReviewScreenshotMaxBytes); err == nil {
			return v
		}
	}
	return reviewScreenshotMaxBytes
}

// The system instruction, in two forms, built on the prompt skeleton in
// docs/gemini-3.5-flash-ui-review-prompting.md: the consistency rules that
// replace temperature/top_p/top_k (which must not be set for Gemini 3.x),
// and the JSON shape the answer must take. A vision model with no stated
// standard judges the page against general web-design convention and reports
// deliberate choices as breakage, so a call carrying a spec is told the spec
// is the only standard, and a call without one is held to defects visible on
// their own terms. Both carry a confidence the caller can weigh, and both
// state that an empty list is an answer
// (docs/reviews/sess-b949743ff7766606eb210ae59f2c1bcd.md). Both name the
// image each finding is about: every image part is preceded by a label part
// ("Image 1: <base name>", internal/gemini), and the model is told to echo
// it, so a finding on a multi-image review says which screenshot it concerns.
const reviewScreenshotSpecInstruction = `You are reviewing a web page screenshot against the design spec sent with it.
The spec is the only standard of correctness. Report a discrepancy only where the screenshot contradicts it; anything the spec does not cover is intentional, so do not flag it against general web-design convention.
Be precise and concise — name the element and what differs (position, size, colour, spacing), not general impressions.
Each image is introduced by a label like "Image 1: home-dark.png"; every finding names the image it concerns, exactly as that label writes it.
Returning an empty list is a valid and expected answer: [] means the screenshot matches the spec.
Output as a JSON list: [{ "image": "", "element": "", "issue": "", "expected": "", "actual": "", "confidence": "high" }]
Set confidence to high only when the spec states the expectation you are measuring against, medium when you are inferring it, and low when the element is too small or the image too ambiguous to be sure.`

const reviewScreenshotNoSpecInstruction = `You are reviewing a web page screenshot for defects.
No design spec was sent with it, so you cannot know what the page is meant to look like: report only what is broken on its own terms — overlapping text, content clipped or overflowing its container, elements outside the viewport, unreadable contrast. Do not report stylistic choices, layout you would have made differently, or anything you are only guessing is wrong.
Be precise and concise — name the element and what is wrong, not general impressions.
Each image is introduced by a label like "Image 1: home-dark.png"; every finding names the image it concerns, exactly as that label writes it.
Returning an empty list is a valid and expected answer: [] means you found no defect.
Output as a JSON list: [{ "image": "", "element": "", "issue": "", "expected": "", "actual": "", "confidence": "high" }]
Set confidence to high only when the defect is unmistakable in the image, medium when it is likely, and low when the element is too small or the image too ambiguous to be sure.`

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
	maxImages := e.reviewScreenshotMaxImages(ctx)
	if len(args.ImagePaths) > maxImages {
		return errorResult("ReviewScreenshot accepts at most %d images, got %d", maxImages, len(args.ImagePaths))
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
		maxBytes := int64(e.reviewScreenshotMaxBytes(ctx))
		if info.Size() > maxBytes {
			return errorResult("screenshot %s is %d bytes, over the %d-byte per-file limit", userPath, info.Size(), maxBytes)
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
		// The label is the file's base name, so a finding can say which
		// screenshot it concerns ("Image 1: home-dark.png") and the human
		// reading the transcript can cross-check it against the rendered
		// image (docs/TOOLS.md, "Seeing the screenshots").
		images = append(images, gemini.Image{Data: data, MIMEType: mimeType, Resolution: resolution, Label: filepath.Base(path)})
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
	instruction := reviewScreenshotNoSpecInstruction
	if args.Spec != "" {
		instruction = reviewScreenshotSpecInstruction
		// The spec is data, so it precedes the question ("data first,
		// question last", docs/gemini-3.5-flash-ui-review-prompting.md).
		question = "Design spec / target CSS:\n" + args.Spec + "\n\n" + question
	}

	answer, usage, err := e.Gemini.Interact(ctx, model, instruction, question, images)
	if err != nil {
		return errorResult("%v", err)
	}
	out, truncated := formatReviewAnswer(answer, e.outputCap(ctx))
	res := Result{Content: out, Truncated: truncated}
	if usage != nil {
		res.GeminiUsage = geminiUsagePayload(e.Prices, model, usage)
	}
	return res
}

// formatReviewAnswer turns Gemini's answer into the tool result text. The
// answer is a JSON array, and the plain byte-cap truncation that labels other
// tools' output would cut it mid-document — DeepSeek would receive a JSON
// list with no closing bracket and read it as broken data. So the array is
// parsed first, and the cap is applied by dropping whole findings off the
// end, with a trailing line saying how many were dropped. The result leads
// with a count line ("3 findings, 2 high confidence") so the model sees the
// shape of the answer before the detail.
//
// An answer that does not parse as a JSON array is returned as the raw text,
// labelled as unparsed prose rather than passed off as JSON. An empty array
// keeps its meaning — a valid answer that found nothing, not a failure the
// model should retry. The boolean reports whether anything was dropped.
func formatReviewAnswer(answer string, cap int) (string, bool) {
	var findings []json.RawMessage
	if err := json.Unmarshal([]byte(answer), &findings); err != nil {
		out, cut := truncate(answer, cap)
		return "Gemini's answer was not a JSON list; unparsed text follows:\n" + out, cut
	}

	high := 0
	for _, f := range findings {
		var m map[string]any
		if json.Unmarshal(f, &m) == nil {
			if c, _ := m["confidence"].(string); c == "high" {
				high++
			}
		}
	}
	// The count line always describes the full answer, even when findings are
	// dropped below it: the trailing note says how many were dropped, so the
	// two lines together tell the model the whole shape.
	header := fmt.Sprintf("%d %s, %d high confidence", len(findings), pluralFindings(len(findings)), high)

	kept := len(findings)
	for {
		body, err := json.MarshalIndent(findings[:kept], "", "  ")
		if err != nil {
			// The model's answer parsed; it can only fail to re-marshal if it
			// holds something no valid JSON array can (it cannot). Fall back
			// to the unparsed label rather than panicking.
			out, cut := truncate(answer, cap)
			return "Gemini's answer was not a JSON list; unparsed text follows:\n" + out, cut
		}
		dropped := len(findings) - kept
		text := header + "\n" + string(body)
		if dropped > 0 {
			text += fmt.Sprintf("\n\n[truncated: dropped %d of %d findings to fit the output cap]", dropped, len(findings))
		}
		if len(text) <= cap || kept == 0 {
			return text, dropped > 0
		}
		kept--
	}
}

func pluralFindings(n int) string {
	if n == 1 {
		return "finding"
	}
	return "findings"
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
