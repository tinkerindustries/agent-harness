package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/mrgeoffrich/deepseek-harness/internal/gemini"
)

type askVisionArgs struct {
	ImagePaths    []string `json:"image_paths"`
	Prompt        string   `json:"prompt"`
	ThinkingLevel string   `json:"thinking_level"`
}

// AskVision is the unstructured half of the vision path. ReviewScreenshot
// asks one question — "what is wrong with this page" — and spends a lot of
// machinery making the answer safe to act on: a fixed instruction, a findings
// schema, an "observed" sentence, entry-wise truncation. That machinery is
// what makes it good at its question and unable to ask any other. Every time
// a session has needed a differently-shaped answer it has either been refused
// or has smuggled the question through the review path, where Gemini answered
// in some other shape and the harness reported it as findings anyway — a
// verbatim transcription once came back as "15 findings, 0 high confidence"
// (docs/reviews/vision-path-2026-08-14.md). describe mode was the first
// carve-out for that pressure. This tool is the general one.
//
// The trade is explicit: the caller writes the whole prompt and gets the
// model's text back with nothing imposed on it, so nothing downstream can
// misread the answer's shape — there is no shape to misread. What the caller
// gives up is every guarantee ReviewScreenshot buys, which is why that tool
// stays and this does not replace it. The tool description carries the
// prompting rules the caller now owns, distilled from
// docs/gemini-3.5-flash-ui-review-prompting.md.
//
// One rule is not left to the caller. The system instruction asks for a
// sentence on what is actually visible before the answer, because the failure
// it prevents is the expensive one and it is invisible from the caller's side:
// a reader who cannot open the image cannot tell a correct answer from an
// answer about a blank page, and a model that cannot tell those apart re-asks.
// One measured session spent 23% of its whole cost establishing that a clean
// answer was clean, and telling the model "an empty answer is a real answer"
// did not fix it — the model was not disbelieving the sentence, it was
// correctly observing that the result carried no evidence
// (docs/reviews/vision-path-2026-08-14.md, and the same reasoning in
// reviewscreenshot.go). Everything else about the answer is the caller's.
const askVisionInstruction = `Answer the question you are asked about the images, in plain text.

Open with one sentence saying what is actually visible in each image, then answer. The reader cannot see the images, so that sentence is the only evidence they have that you looked at what they think you looked at.

Be specific and concise: name elements, positions, sizes, colours and text you can actually point to. Do not pad the answer with general impressions or advice that is not asked for.`

// execAskVision sends the caller's own prompt to the vision model and returns
// the reply as text. The image handling, the caps and the cost line are
// ReviewScreenshot's, unchanged — only the prompting and the answer handling
// differ.
func execAskVision(ctx context.Context, e *Executor, argsRaw json.RawMessage) Result {
	var args askVisionArgs
	if err := json.Unmarshal(argsRaw, &args); err != nil {
		return errorResult("invalid arguments: %v", err)
	}
	if len(args.ImagePaths) == 0 {
		return errorResult("image_paths is required and must name at least one file")
	}
	if args.Prompt == "" {
		return errorResult("prompt is required")
	}
	level, err := askVisionThinkingLevel(args.ThinkingLevel)
	if err != nil {
		return errorResult("%v", err)
	}
	maxImages := e.reviewScreenshotMaxImages(ctx)
	if len(args.ImagePaths) > maxImages {
		return errorResult("AskVision accepts at most %d images, got %d", maxImages, len(args.ImagePaths))
	}

	images, downscaled, err := loadReviewImages(ctx, e, args.ImagePaths)
	if err != nil {
		return errorResult("%v", err)
	}
	// After argument validation, so a bad extension or an undecodable image is
	// reported as itself even in a session with no Gemini client — that is the
	// error the model can route around (the same ordering as
	// execReviewScreenshot).
	if e.Gemini == nil {
		return errorResult("AskVision is not available in this context: no Gemini client configured")
	}

	model := gemini.DefaultModel
	if e.GeminiModel != nil {
		if m, err := e.GeminiModel(); err != nil {
			return errorResult("resolve vision model: %v", err)
		} else if m != "" {
			model = m
		}
	}

	// The instant the request went out, which is what the price table
	// costs against (internal/pricing.Table.Cost).
	sentAt := time.Now()
	answer, usage, err := e.Gemini.Interact(ctx, model, askVisionInstruction, args.Prompt, images,
		gemini.WithThinkingLevel(level),
		// Same measured reason as ReviewScreenshot: the field carries a type
		// and no schema, so constraining the container empties or mangles the
		// contents. Here it would be doubly wrong — the answer is meant to be
		// prose (docs/gemini-3.5-flash-ui-review-prompting.md).
		gemini.WithResponseFormat(""))
	if err != nil {
		return errorResult("%v", err)
	}

	// Plain byte truncation is safe here in a way it is not for
	// ReviewScreenshot: there is no JSON document to sever, so a cut answer is
	// a short answer rather than a broken one.
	out, truncated := truncate(answer, e.outputCap(ctx))
	content := out
	if len(downscaled) > 0 {
		// Ahead of the answer, so the model knows the vision model saw less
		// detail than the file holds before it reads what it concluded.
		content = strings.Join(downscaled, "\n") + "\n\n" + content
	}
	res := Result{Content: content, Truncated: truncated}
	if usage != nil {
		// Recorded and reported exactly as a review call's is: the usage rides
		// on the result so the vision spend is a separable event in the log,
		// and the figure comes home in the text because the description cannot
		// carry a number (docs/CACHE.md) and "expensive" on its own has not
		// stopped a session spending a third of its budget here.
		res.GeminiUsage = geminiUsagePayload(e.Prices, model, sentAt, usage)
		if line := reviewCostLine(res.GeminiUsage, model); line != "" {
			res.Content += "\n\n" + line
		}
	}
	return res
}

// askVisionThinkingLevel validates the caller's choice. The level is exposed
// because it is the one Gemini knob that changes answer quality for this use
// case and the caller knows which kind of question it is asking: a "does this
// roughly match" pass wants low, a subtle layout bug wants high
// (docs/gemini-3.5-flash-ui-review-prompting.md). Omitted, it takes the same
// default a review call takes, so a caller that does not care does not have to
// decide.
func askVisionThinkingLevel(level string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "":
		return gemini.ThinkingLevelMedium, nil
	case gemini.ThinkingLevelLow:
		return gemini.ThinkingLevelLow, nil
	case gemini.ThinkingLevelMedium:
		return gemini.ThinkingLevelMedium, nil
	case gemini.ThinkingLevelHigh:
		return gemini.ThinkingLevelHigh, nil
	}
	return "", fmt.Errorf("thinking_level must be low, medium, or high, got %q", level)
}
