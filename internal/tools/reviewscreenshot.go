package tools

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"golang.org/x/image/draw"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"time"
	// Registers the WebP decoder with image.Decode; x/image has no WebP
	// encoder, so oversize WebP files are re-encoded as PNG (encodeScreenshot).
	_ "golang.org/x/image/webp"

	"github.com/mrgeoffrich/deepseek-harness/internal/gemini"
	"github.com/mrgeoffrich/deepseek-harness/internal/pricing"
	"github.com/mrgeoffrich/deepseek-harness/internal/settings"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

type reviewScreenshotArgs struct {
	ImagePaths     []string `json:"image_paths"`
	Question       string   `json:"question"`
	Spec           string   `json:"spec"`
	Mode           string   `json:"mode"`
	ConversationID string   `json:"conversation_id"`
}

// The two things a caller can ask the vision model for. Review judges the
// screenshots and reports what is wrong with them; describe says what is on
// them and makes no judgement at all.
//
// Describe exists because the model kept asking for it through the review
// path and the review path kept refusing: a "does this page have content on
// it at all" question has no place in a findings schema, so it came back as
// an empty list, which reads identically to "the page is perfect"
// (docs/reviews/vision-path-2026-08-14.md). Left without the mode, DeepSeek
// worked around it by asking for a verbatim transcription in a review call —
// which Gemini answered by abandoning the findings shape, and which the
// harness then reported as "15 findings, 0 high confidence".
const (
	reviewModeReview   = "review"
	reviewModeDescribe = "describe"
)

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

// The system instruction, in three forms, built on the prompt skeleton in
// docs/gemini-3.5-flash-ui-review-prompting.md: the consistency rules that
// replace temperature/top_p/top_k (which must not be set for Gemini 3.x),
// and the JSON shape the answer must take. A vision model with no stated
// standard judges the page against general web-design convention and reports
// deliberate choices as breakage, so a call carrying a spec is told the spec
// is the only standard, and a call without one is held to defects visible on
// their own terms. The third is describe mode, which judges nothing.
//
// All three open with "observed", and that field is the point. An empty
// findings list and a blank page produced identical bytes, so DeepSeek —
// which cannot see the image to check — could not tell "the page is correct"
// from "the page never rendered", and did not believe either. It said so in
// its own reasoning, re-asked the same screenshot, and in one measured
// session burned five calls and 23% of the run's whole cost establishing
// that a clean answer was clean
// (docs/reviews/vision-path-2026-08-14.md). Telling the model an empty list
// is an answer was the previous attempt at this and did not work
// (docs/reviews/sess-b949743ff7766606eb210ae59f2c1bcd.md): the model was not
// disbelieving the sentence, it was correctly observing that the result
// carried no evidence. So every answer now carries a plain-language sentence
// on what is actually on the screen, and "0 findings" arrives attached to
// something a reader can check it against.
//
// All three also name the image each entry is about: every image part is
// preceded by a label part ("Image 1: <base name>", internal/gemini), and the
// model is told to echo it, so an entry on a multi-image call says which
// screenshot it concerns.
const reviewScreenshotSpecInstruction = `You are reviewing a web page screenshot against the design spec sent with it.
The spec is the only standard of correctness. Report a discrepancy only where the screenshot contradicts it; anything the spec does not cover is intentional, so do not flag it against general web-design convention.
Be precise and concise — name the element and what differs (position, size, colour, spacing), not general impressions.
Each image is introduced by a label like "Image 1: home-dark.png"; every finding names the image it concerns, exactly as that label writes it.
Output a JSON object: { "observed": "", "findings": [{ "image": "", "element": "", "issue": "", "expected": "", "actual": "", "confidence": "high" }] }
"observed" is required and is never empty: one or two plain sentences saying what is actually on the screen — the main regions and what they contain, per image if there is more than one. If an image is blank, is an error page, or failed to render, say exactly that in "observed". The person reading your answer cannot see the image, so this sentence is their only evidence that you were looking at the right thing.
An empty "findings" list is a valid and expected answer: it means the screenshot matches the spec, and "observed" is what shows you checked.
Set confidence to high only when the spec states the expectation you are measuring against, medium when you are inferring it, and low when the element is too small or the image too ambiguous to be sure.`

const reviewScreenshotNoSpecInstruction = `You are reviewing a web page screenshot for defects.
No design spec was sent with it, so you cannot know what the page is meant to look like: report only what is broken on its own terms — overlapping text, content clipped or overflowing its container, elements outside the viewport, unreadable contrast. Do not report stylistic choices, layout you would have made differently, or anything you are only guessing is wrong.
Be precise and concise — name the element and what is wrong, not general impressions.
Each image is introduced by a label like "Image 1: home-dark.png"; every finding names the image it concerns, exactly as that label writes it.
Output a JSON object: { "observed": "", "findings": [{ "image": "", "element": "", "issue": "", "expected": "", "actual": "", "confidence": "high" }] }
"observed" is required and is never empty: one or two plain sentences saying what is actually on the screen — the main regions and what they contain, per image if there is more than one. If an image is blank, is an error page, or failed to render, say exactly that in "observed". The person reading your answer cannot see the image, so this sentence is their only evidence that you were looking at the right thing.
An empty "findings" list is a valid and expected answer: it means you found no defect, and "observed" is what shows you looked.
Set confidence to high only when the defect is unmistakable in the image, medium when it is likely, and low when the element is too small or the image too ambiguous to be sure.`

// Describe mode. It reports and does not judge: the caller has asked what is
// on the screen, so a defect list here would be answering a question nobody
// asked, and the "do not guess" rules that keep review honest would suppress
// exactly the detail being requested.
const reviewScreenshotDescribeInstruction = `You are describing a web page screenshot for someone who cannot see it.
Report what is there. Do not judge the design, do not look for defects, and do not say what the page should have looked like — you have not been told, and that is not what was asked.
Each image is introduced by a label like "Image 1: home-dark.png"; every element names the image it appears in, exactly as that label writes it.
Output a JSON object: { "observed": "", "elements": [{ "image": "", "text": "", "role": "", "styling": "" }] }
"observed" is required and is never empty: one or two plain sentences on the overall layout — the main regions, top to bottom, and what each holds. If an image is blank, is an error page, or failed to render, say exactly that in "observed" and return an empty "elements" list.
"elements" is the detail, in reading order: "text" is the visible text transcribed verbatim (empty for an element that has none), "role" names what it is (heading, button, table cell, badge, icon), and "styling" is how it is rendered — bold, dim, colour, background, size relative to its neighbours — in a few words.
Transcribe only text you can actually read. Where text is too small or blurred to read, say so in "styling" rather than guessing at the characters.`

// execReviewScreenshot implements ReviewScreenshot: send one to four
// screenshots to Google Gemini's vision model and return its diagnosis of
// the question. The first image goes at high resolution and the rest at
// medium, per the doc's advice that only the image needing scrutiny should
// be high — which is why the tool description tells the model to put the
// screenshot it cares about first. A nil Gemini client is an ordinary error
// result, not a panic and not a failed run: the same shape WebFetch uses for
// a nil e.Client.
//
// A call without conversation_id starts a conversation and its result
// carries the id; a call passing one and a question continues it. The
// conversation is held on the Executor (per-session) and stores the image
// paths and the prior question/answer pairs — never the image bytes — so a
// follow-up re-reads the files from disk, and a file that has since been
// deleted is an ordinary error result naming the missing path. "Look closer
// at the header" therefore costs one Gemini interaction with the images
// re-sent, not a fresh review from scratch.
//
// A follow-up may also carry new image_paths, which replace the
// conversation's stored set. That is the loop the tool is actually used in —
// review, change something, capture it again, ask whether it is fixed — and
// it used to be the one shape refused, so a model that had re-captured a
// page got its previous answer back, about the previous bytes, and paid for
// it. One production session diagnosed that itself and started a fresh
// conversation instead; the point of allowing it here is that the thread of
// earlier questions and answers survives the re-capture
// (docs/reviews/vision-path-2026-08-14.md).
func execReviewScreenshot(ctx context.Context, e *Executor, argsRaw json.RawMessage) Result {
	var args reviewScreenshotArgs
	if err := json.Unmarshal(argsRaw, &args); err != nil {
		return errorResult("invalid arguments: %v", err)
	}

	var conversation *reviewConversation
	paths := args.ImagePaths
	spec := args.Spec
	// imagesReplaced records that this follow-up brought its own screenshots,
	// which the question sent to Gemini has to say out loud: the thread it is
	// replayed alongside describes different bytes, and without the warning
	// the model reconciles the new image against its own earlier answer.
	imagesReplaced := false
	if args.ConversationID == "" {
		if len(args.ImagePaths) == 0 {
			return errorResult("image_paths is required and must name at least one file")
		}
	} else {
		conversation = e.reviewConversation(args.ConversationID)
		if conversation == nil {
			return errorResult("unknown conversation_id %q — start a new conversation by omitting it", args.ConversationID)
		}
		if args.Spec != "" {
			return errorResult("a follow-up call takes no spec: the spec is fixed when the conversation starts")
		}
		if args.Mode != "" && !strings.EqualFold(args.Mode, conversation.mode) {
			return errorResult("a follow-up call takes no mode: this conversation is in %s mode, fixed when it started", conversation.mode)
		}
		spec = conversation.spec
		if len(args.ImagePaths) > 0 {
			imagesReplaced = true
		} else {
			paths = conversation.imagePaths
		}
	}
	if args.Question == "" {
		return errorResult("question is required")
	}
	mode, err := reviewMode(args.Mode, conversation)
	if err != nil {
		return errorResult("%v", err)
	}
	maxImages := e.reviewScreenshotMaxImages(ctx)
	if len(paths) > maxImages {
		return errorResult("ReviewScreenshot accepts at most %d images, got %d", maxImages, len(paths))
	}

	images, downscaled, err := loadReviewImages(ctx, e, paths)
	if err != nil {
		return errorResult("%v", err)
	}

	// The capability check comes after argument validation, so a call with a
	// bad extension or an image that will not decode learns that even when
	// this session has no Gemini client — the specific error is the one the
	// model can route around. An oversize file that does decode is downscaled
	// here and then fails the capability check below like any other image.
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

	instruction := reviewScreenshotDescribeInstruction
	question := args.Question
	if mode == reviewModeReview {
		instruction = reviewScreenshotNoSpecInstruction
		if spec != "" {
			instruction = reviewScreenshotSpecInstruction
			// The spec is data, so it precedes the question ("data first,
			// question last", docs/gemini-3.5-flash-ui-review-prompting.md).
			question = "Design spec / target CSS:\n" + spec + "\n\n" + question
		}
	}
	if conversation != nil {
		// A follow-up re-sends what the conversation has established — the
		// earlier questions and Gemini's answers, ahead of the new question —
		// so the model answers with the whole thread in front of it.
		question = reviewFollowUpQuestion(conversation, spec, args.Question, imagesReplaced)
	}

	// The instant the request went out, which is what the price table
	// costs against (internal/pricing.Table.Cost).
	sentAt := time.Now()
	answer, usage, err := e.Gemini.Interact(ctx, model, instruction, question, images,
		gemini.WithThinkingLevel(e.visionThinkingLevel(ctx, mode)),
		// No response_format at all: the model decides its own container, and
		// reads the instruction to know what to put in it. This is measured
		// rather than assumed (docs/gemini-3.5-flash-ui-review-prompting.md,
		// "response_format is a type, not a schema"). The field carries a
		// type and no schema, so "object" returns a literally empty object
		// and "array" forces the top level to be a list and mangles the
		// object inside it. Omitted, the answer comes back exactly as the
		// instruction asked — inside a ```json fence, which
		// formatReviewAnswer strips. A fence is a far smaller problem than an
		// empty answer.
		gemini.WithResponseFormat(""))
	if err != nil {
		return errorResult("%v", err)
	}

	turn := reviewTurn{question: args.Question, answer: answer}
	conversationID := args.ConversationID
	if conversationID == "" {
		conversationID = e.startReviewConversation(paths, spec, mode, turn)
	} else {
		e.appendReviewTurn(conversationID, paths, turn)
	}

	out, truncated := formatReviewAnswer(answer, mode, e.outputCap(ctx))
	content := "conversation_id: " + conversationID + "\n\n" + out
	if len(downscaled) > 0 {
		// Lead with the downscale notes so the model reads them before the
		// findings: each says which file was shrunk and to what dimensions,
		// so the model knows the vision model saw less detail than the file
		// holds (and can say so in its answer if that matters).
		content = strings.Join(downscaled, "\n") + "\n\n" + content
	}
	res := Result{Content: content, Truncated: truncated}
	if usage != nil {
		res.GeminiUsage = geminiUsagePayload(e.Prices, model, sentAt, usage)
		// What the call cost, on the result, in the place the model decides
		// whether to make another one. The tool description can only say a
		// review is expensive in general — it is part of the frozen request
		// head, so it cannot carry a number that varies by installation
		// (docs/CACHE.md) — and "expensive" did not stop a session spending
		// 39% of its budget here. The actual figure can only come home on the
		// result, which is why it does.
		if line := reviewCostLine(res.GeminiUsage, model); line != "" {
			res.Content += "\n\n" + line
		}
	}
	return res
}

// reviewCostLine states what one Gemini call cost, and what the thinking
// share of it was. Thinking is the part a caller can actually do something
// about: it bills at the output rate and dominates — an empty findings list
// has been measured at 1,947 thinking tokens against a one-token answer — so
// naming it points at the mode and the thinking-level setting rather than at
// the call count alone.
//
// A price table with no entry for the vision model leaves the cost at zero
// (geminiUsagePayload), and a zero cost prints no line: an invented $0.0000
// would read as "this was free", which is the opposite of true.
func reviewCostLine(usage *store.UsagePayload, model string) string {
	if usage == nil || usage.CostUSD <= 0 {
		return ""
	}
	line := fmt.Sprintf("This call cost $%.4f on %s", usage.CostUSD, model)
	if usage.ReasoningTokens > 0 && usage.CompletionTokens > 0 {
		line += fmt.Sprintf(" (%d of %d output tokens were the model thinking)",
			usage.ReasoningTokens, usage.CompletionTokens)
	}
	return line + "."
}

// reviewMode resolves the mode argument, defaulting to review and holding a
// follow-up to the mode its conversation started in — the instruction is
// fixed for a conversation's life the same way the spec is, because the
// thread being replayed was answered under it.
func reviewMode(raw string, conversation *reviewConversation) (string, error) {
	if conversation != nil && conversation.mode != "" {
		return conversation.mode, nil
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return reviewModeReview, nil
	case reviewModeReview:
		return reviewModeReview, nil
	case reviewModeDescribe:
		return reviewModeDescribe, nil
	default:
		return "", fmt.Errorf("mode must be %q or %q, got %q", reviewModeReview, reviewModeDescribe, raw)
	}
}

// visionThinkingLevel decides how hard the vision model thinks. The operator
// can pin it (google.vision_thinking_level); "auto", the default, lets the
// mode choose — judging a page against a spec is reasoning, and saying what
// is on a page is not.
func (e *Executor) visionThinkingLevel(ctx context.Context, mode string) string {
	if e.Settings != nil {
		if v, err := e.Settings.String(ctx, settings.KeyGoogleVisionThinkingLevel); err == nil && v != "" && v != "auto" {
			return v
		}
	}
	if mode == reviewModeDescribe {
		return gemini.ThinkingLevelLow
	}
	return gemini.ThinkingLevelMedium
}

// loadReviewImages reads and validates the image files paths names — the
// same checks the first call applies, re-run on a follow-up, so a file that
// has been deleted, grown over the byte cap, or renamed to a bad extension
// since the conversation started fails with an ordinary error naming it.
// The first image goes at high resolution and the rest at medium, and each
// image's label is its file's base name.
//
// A file over the byte cap is downscaled rather than refused: it is decoded,
// shrunk preserving aspect ratio until the re-encoded bytes fit under the
// cap, and the shrunk bytes are sent. The second return value carries one
// note per downscaled file — which file, and to what dimensions — so the
// model knows the vision model saw less detail than the file holds. A file
// that will not decode keeps the refusal, naming the limit and the reason.
// A file already under the cap is sent byte-identically: it is never decoded
// and re-encoded.
func loadReviewImages(ctx context.Context, e *Executor, paths []string) ([]gemini.Image, []string, error) {
	images := make([]gemini.Image, 0, len(paths))
	var downscaled []string
	for i, userPath := range paths {
		// resolveImagePath rather than ResolvePath: a relative path that names
		// nothing is tried once more under scratch/, which is where Screenshot
		// puts a relative capture, so the path the model wrote the image to is
		// the path it can read it back from (workspace.go).
		path, err := resolveImagePath(e.Workspace, userPath)
		if err != nil {
			return nil, nil, err
		}
		info, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, nil, fmt.Errorf("file not found: %s", userPath)
			}
			return nil, nil, fmt.Errorf("stat %s: %v", userPath, err)
		}
		if info.IsDir() {
			return nil, nil, fmt.Errorf("%s is a directory, not a screenshot", userPath)
		}
		mimeType, ok := screenshotMIMEType(path)
		if !ok {
			return nil, nil, fmt.Errorf("unsupported screenshot type for %s: ReviewScreenshot accepts PNG, JPEG, and WebP files", userPath)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, nil, fmt.Errorf("read %s: %v", userPath, err)
		}

		maxBytes := int64(e.reviewScreenshotMaxBytes(ctx))
		if int64(len(data)) > maxBytes {
			shrunk, shrunkMIME, w, h, err := downscaleImage(data, mimeType, maxBytes)
			if err != nil {
				// The refusal names both the limit and why the route around it
				// (downscaling) was not available, so the model knows a re-capture
				// is the only way forward rather than retrying the same file.
				return nil, nil, fmt.Errorf("screenshot %s is %d bytes, over the %d-byte per-file limit, and could not be downscaled: %v", userPath, len(data), maxBytes, err)
			}
			note := fmt.Sprintf("%s was downscaled to %dx%d to fit the %d-byte per-file limit", userPath, w, h, maxBytes)
			if shrunkMIME != mimeType {
				note += fmt.Sprintf(" (re-encoded as %s: x/image has no WebP encoder)", strings.TrimPrefix(shrunkMIME, "image/"))
			}
			data = shrunk
			mimeType = shrunkMIME
			downscaled = append(downscaled, note)
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
	return images, downscaled, nil
}

// downscaleImage decodes data (a PNG, JPEG, or WebP) and re-encodes it at
// halved dimensions, preserving aspect ratio, until the bytes fit under cap.
// It returns the shrunk bytes, the MIME type they are encoded as, and the
// dimensions it settled on. The standard library has no scaler, so the
// resample uses x/image's CatmullRom, a quality-preserving resampler;
// encoding quality is fixed (jpeg), so the loop converges by dimension alone.
// x/image decodes WebP but has no WebP encoder, so a WebP file comes back as
// PNG — the only re-encode that keeps alpha. An image that will not decode
// at all, or that still does not fit at the smallest size, is an error the
// caller turns into the refusal.
func downscaleImage(data []byte, mimeType string, cap int64) ([]byte, string, int, int, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", 0, 0, err
	}
	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	if w < 1 || h < 1 {
		return nil, "", 0, 0, fmt.Errorf("image has no pixels")
	}
	for w > 1 && h > 1 {
		w, h = w/2, h/2
		dst := image.NewRGBA(image.Rect(0, 0, w, h))
		draw.CatmullRom.Scale(dst, dst.Bounds(), img, bounds, draw.Over, nil)
		encoded, encodedMIME, err := encodeScreenshot(dst, mimeType)
		if err != nil {
			return nil, "", 0, 0, err
		}
		if int64(len(encoded)) <= cap {
			return encoded, encodedMIME, w, h, nil
		}
	}
	return nil, "", 0, 0, fmt.Errorf("still over the byte cap at the smallest size")
}

// encodeScreenshot re-encodes an image in the format mimeType names, with a
// quality that keeps a screenshot reviewable without inflating the bytes.
// WebP has no encoder in x/image, so it is re-encoded as PNG (which keeps
// alpha); the returned MIME type is what the bytes actually are.
func encodeScreenshot(img image.Image, mimeType string) ([]byte, string, error) {
	var buf bytes.Buffer
	switch mimeType {
	case "image/png", "image/webp":
		err := png.Encode(&buf, img)
		return buf.Bytes(), "image/png", err
	case "image/jpeg":
		err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85})
		return buf.Bytes(), "image/jpeg", err
	default:
		return nil, "", fmt.Errorf("cannot re-encode %s", mimeType)
	}
}

// reviewFollowUpQuestion composes a follow-up's question: the conversation's
// prior question/answer pairs (and the spec, when the conversation has one),
// with the new question last — data first, question last, the same shape as
// a first call.
// imagesReplaced says the screenshots attached to this call are not the ones
// the thread above describes, which the model has to be told: replayed
// alongside its own earlier answer, a fresh capture otherwise gets reconciled
// against that answer instead of being looked at.
func reviewFollowUpQuestion(conversation *reviewConversation, spec, question string, imagesReplaced bool) string {
	var b strings.Builder
	if spec != "" {
		b.WriteString("Design spec / target CSS:\n")
		b.WriteString(spec)
		b.WriteString("\n\n")
	}
	if len(conversation.turns) > 0 {
		b.WriteString("Earlier in this conversation:\n")
		for _, turn := range conversation.turns {
			fmt.Fprintf(&b, "Question: %s\nAnswer: %s\n\n", turn.question, turn.answer)
		}
	}
	if imagesReplaced {
		b.WriteString("The screenshots attached to this message are NEW captures, taken after the exchange above. They replace the ones you were shown earlier. Judge what you can see now; treat the earlier exchange as history, not as a description of these images.\n\n")
	}
	b.WriteString(question)
	return b.String()
}

// reviewAnswer is the envelope all three instructions ask for. Findings and
// Elements are the review and describe halves of the same slot — only one is
// ever populated, because only one instruction was sent — so the parse takes
// whichever arrived.
type reviewAnswer struct {
	Observed string            `json:"observed"`
	Findings []json.RawMessage `json:"findings"`
	Elements []json.RawMessage `json:"elements"`
}

// formatReviewAnswer turns Gemini's answer into the tool result text.
//
// The answer is a JSON object, and the plain byte-cap truncation that labels
// other tools' output would cut it mid-document — DeepSeek would receive JSON
// with no closing brace and read it as broken data. So it is parsed first,
// and the cap is applied by dropping whole entries off the end, with a
// trailing line saying how many went.
//
// The result leads with what the vision model says it saw, then the count.
// That ordering is the fix this whole shape exists for: "0 findings" on its
// own is indistinguishable from a blank page to a reader who cannot open the
// image, and a model that cannot tell those apart re-asks — which is
// measurably more expensive than the review was
// (docs/reviews/vision-path-2026-08-14.md). With the sentence above it the
// count becomes checkable, and a genuinely blank capture says so in words.
//
// Three shapes degrade rather than fail, because the answer comes from a
// model and not from a schema the API enforces: a bare JSON array is the
// pre-observed shape and is still read as findings, with a note that the
// description is missing; an object that parses but has no "observed" gets
// the same note; anything that is not JSON at all comes back as raw text,
// labelled as unparsed prose rather than passed off as structure. The
// boolean reports whether anything was dropped.
func formatReviewAnswer(answer, mode string, cap int) (string, bool) {
	unparsed := func() (string, bool) {
		out, cut := truncate(answer, cap)
		return "Gemini's answer was not in the expected JSON shape; unparsed text follows:\n" + out, cut
	}

	var parsed reviewAnswer
	trimmed := stripJSONFence(answer)
	switch {
	case strings.HasPrefix(trimmed, "{"):
		if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
			return unparsed()
		}
	case strings.HasPrefix(trimmed, "["):
		// The shape before "observed" existed. Still legible, so it is read
		// rather than refused — but the missing description is called out
		// below, because its absence is the thing the reader has to know.
		if err := json.Unmarshal([]byte(trimmed), &parsed.Findings); err != nil {
			return unparsed()
		}
	default:
		return unparsed()
	}

	entries, noun := parsed.Findings, "finding"
	if mode == reviewModeDescribe {
		entries, noun = parsed.Elements, "element"
	}
	// An absent key decodes to a nil slice, and a nil slice marshals to the
	// literal "null" — which reads as a broken answer rather than as an empty
	// one. Empty is the honest rendering and the common case.
	if entries == nil {
		entries = []json.RawMessage{}
	}

	var head string
	if parsed.Observed != "" {
		head = "Observed: " + parsed.Observed + "\n\n"
	} else {
		// Said plainly, because an answer with no description is exactly the
		// case the reader cannot verify — and a silent omission would put
		// them back where they started, weighing a bare count.
		head = "Observed: (the vision model returned no description of what it saw, so this count is not corroborated)\n\n"
	}

	high := 0
	for _, f := range entries {
		var m map[string]any
		if json.Unmarshal(f, &m) == nil {
			if c, _ := m["confidence"].(string); c == "high" {
				high++
			}
		}
	}
	// The count line always describes the full answer, even when entries are
	// dropped below it: the trailing note says how many were dropped, so the
	// two lines together tell the model the whole shape. Confidence is a
	// review idea; describe mode's elements carry none, so it is not quoted.
	header := fmt.Sprintf("%d %s", len(entries), plural(noun, len(entries)))
	if mode != reviewModeDescribe {
		header += fmt.Sprintf(", %d high confidence", high)
	}

	kept := len(entries)
	for {
		body, err := json.MarshalIndent(entries[:kept], "", "  ")
		if err != nil {
			// The model's answer parsed; it can only fail to re-marshal if it
			// holds something no valid JSON can (it cannot). Fall back to the
			// unparsed label rather than panicking.
			return unparsed()
		}
		dropped := len(entries) - kept
		text := head + header + "\n" + string(body)
		if dropped > 0 {
			text += fmt.Sprintf("\n\n[truncated: dropped %d of %d %s to fit the output cap]",
				dropped, len(entries), plural(noun, len(entries)))
		}
		if len(text) <= cap || kept == 0 {
			return text, dropped > 0
		}
		kept--
	}
}

// stripJSONFence removes a leading ```json (or bare ```) fence and its
// closing counterpart, returning the trimmed remainder.
//
// The fence is the price of letting the model choose its own container, and
// it is the right trade: constraining the container costs the contents, while
// leaving it alone gets the exact shape asked for and wraps it in three
// backticks (docs/gemini-3.5-flash-ui-review-prompting.md). Text that is not
// fenced comes back trimmed and otherwise untouched, so this is safe to run
// over every answer.
func stripJSONFence(answer string) string {
	trimmed := strings.TrimSpace(answer)
	if !strings.HasPrefix(trimmed, "```") {
		return trimmed
	}
	// Drop the opening fence line, whatever language tag it carries.
	if _, rest, ok := strings.Cut(trimmed, "\n"); ok {
		trimmed = rest
	} else {
		return trimmed
	}
	if i := strings.LastIndex(trimmed, "```"); i >= 0 {
		trimmed = trimmed[:i]
	}
	return strings.TrimSpace(trimmed)
}

func plural(noun string, n int) string {
	if n == 1 {
		return noun
	}
	return noun + "s"
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
// billedAt is the instant the vision request was made. Gemini bills one rate
// around the clock, so no Gemini model appears in the price table's rate
// schedule and the tier is always flat — but Cost still requires the instant
// rather than assuming one, because a signature that lets a caller omit it is
// a signature the DeepSeek path could have omitted it on too.
func geminiUsagePayload(prices *pricing.Table, model string, billedAt time.Time, usage *gemini.Usage) *store.UsagePayload {
	cacheHit, cacheMiss, completion, reasoning := usage.TokenSplit()
	payload := &store.UsagePayload{
		// Named, so this event is separable from the session's own turns. It
		// shares their sub-turn and their shape, and until the field existed
		// the only thing distinguishing a two-cent vision call from a
		// sub-cent DeepSeek turn in the log was the collision.
		Model:                 model,
		PromptTokens:          usage.TotalInputTokens,
		PromptCacheHitTokens:  cacheHit,
		PromptCacheMissTokens: cacheMiss,
		CompletionTokens:      completion,
		ReasoningTokens:       reasoning,
	}
	if prices != nil {
		if c, tier, err := prices.Cost(model, billedAt, cacheHit, cacheMiss, completion); err == nil {
			payload.CostUSD = c
			payload.RateTier = string(tier)
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

// maxReviewConversations caps how many ReviewScreenshot conversations one
// session holds at once. Each conversation is a handful of paths and text
// pairs, so the cap is about bounding a long session's growth, not memory
// pressure; the most recent conversations are kept and the oldest dropped.
const maxReviewConversations = 5

// reviewTurn is one question/answer exchange of a ReviewScreenshot
// conversation. The answer is Gemini's raw text, not the formatted tool
// result: it is what gets re-sent to the model on a follow-up.
type reviewTurn struct {
	question string
	answer   string
}

// reviewConversation is the state a follow-up needs to continue a review
// without re-uploading: the image paths (the files are re-read from disk on
// each follow-up, so the bytes are never held), and the spec and mode, both
// fixed for the conversation's life because the thread of earlier answers was
// produced under them. Stored on the Executor, which belongs to exactly one
// session (docs/DESIGN.md §4.5).
//
// imagePaths is the one part a follow-up may replace, by passing its own
// image_paths — the re-capture loop the tool is used in.
type reviewConversation struct {
	imagePaths []string
	spec       string
	mode       string
	turns      []reviewTurn
}

// reviewConversation returns the conversation with id, or nil.
func (e *Executor) reviewConversation(id string) *reviewConversation {
	e.reviewMu.Lock()
	defer e.reviewMu.Unlock()
	return e.reviewConversations[id]
}

// startReviewConversation records a fresh conversation and returns its id.
// The map is created lazily so an Executor built without the field (every
// test that constructs one directly) works unchanged. When the cap is
// exceeded the oldest conversation is dropped.
func (e *Executor) startReviewConversation(imagePaths []string, spec, mode string, turn reviewTurn) string {
	e.reviewMu.Lock()
	defer e.reviewMu.Unlock()
	if e.reviewConversations == nil {
		e.reviewConversations = make(map[string]*reviewConversation)
	}
	id := newReviewConversationID()
	e.reviewConversations[id] = &reviewConversation{imagePaths: imagePaths, spec: spec, mode: mode, turns: []reviewTurn{turn}}
	e.reviewOrder = append(e.reviewOrder, id)
	if len(e.reviewOrder) > maxReviewConversations {
		oldest := e.reviewOrder[0]
		e.reviewOrder = e.reviewOrder[1:]
		delete(e.reviewConversations, oldest)
	}
	return id
}

// appendReviewTurn records one more exchange on an existing conversation,
// and re-points it at the images that exchange was actually about — so the
// next follow-up continues from the newest capture rather than reverting to
// the ones the conversation opened with.
func (e *Executor) appendReviewTurn(id string, imagePaths []string, turn reviewTurn) {
	e.reviewMu.Lock()
	defer e.reviewMu.Unlock()
	if c := e.reviewConversations[id]; c != nil {
		c.imagePaths = imagePaths
		c.turns = append(c.turns, turn)
	}
}

// newReviewConversationID mints a conversation id. The "rvw-" prefix makes
// its origin obvious in a tool result the model echoes back.
func newReviewConversationID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("tools: crypto/rand unavailable: " + err.Error())
	}
	return "rvw-" + hex.EncodeToString(b[:])
}
