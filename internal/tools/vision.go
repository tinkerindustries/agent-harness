package tools

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/image/draw"

	"github.com/mrgeoffrich/agent-harness/internal/attachment"
	"github.com/mrgeoffrich/agent-harness/internal/gemini"
	"github.com/mrgeoffrich/agent-harness/internal/pricing"
	"github.com/mrgeoffrich/agent-harness/internal/settings"
	"github.com/mrgeoffrich/agent-harness/internal/store"
)

// The vision path: four tools ported from Anionex/agent-vision-toolkit (MIT,
// © 2026 Anionex), which replaced this harness's own ReviewScreenshot and
// AskVision. docs/VISION-TOOLKIT.md is the assessment that led here; the
// short version is that our two tools both answered in prose, and prose is
// the wrong return type for half the questions a session asks about a page.
// Glance answers what something is; Ground and Detect answer where it is, in
// pixels; Crop cuts a box out with no model call at all.
//
// The prompts and the parsing are ported as faithfully as the shapes allow,
// because they are the part that took measuring: the 0-1000 grid, the
// [y0, x0, y1, x1] ordering, the four-stage tolerant parse, and the 8192
// token ceiling are all load-bearing and all recorded here with the reason
// they are what they are. What is deliberately NOT ported is their HTTP
// client: internal/gemini already speaks the native Gemini API with thinking
// levels, retries and usage accounting, where theirs is an OpenAI-compatible
// poster with none.
//
// Two divergences from the upstream CLIs, both deliberate:
//
//   - The LANG instruction (their vision_client.py prepends "Please respond in
//     English" or the Simplified Chinese equivalent from $LANG) is dropped.
//     The harness has no locale concept and the instruction would be dead text
//     in the request head.
//   - Usage, cost, the per-file byte cap and the downscale notice are ours and
//     stay. The CLIs report no token usage at all, and adopting that would
//     have made every vision call invisible to the cost figures
//     (docs/VISION-TOOLKIT.md §3).

// Limits the vision tools enforce on their input. Production resolves both
// through the settings registry; the keys keep their original names
// (tools.reviewscreenshot_max_images, tools.reviewscreenshot_max_bytes)
// because they are stored operator configuration and renaming them would
// silently reset every installation that had set one. The tool descriptions
// name no numbers (docs/CACHE.md: the tool array is part of the frozen
// request head), so the model learns a changed limit from the refusal.
const (
	visionMaxImagesDefault = 4
	visionMaxBytesDefault  = 5 << 20 // 5 MB per file
)

func (e *Executor) visionMaxImages(ctx context.Context) int {
	if e.Settings != nil {
		if v, err := e.Settings.Int(ctx, settings.KeyToolReviewScreenshotMaxImages); err == nil {
			return v
		}
	}
	return visionMaxImagesDefault
}

func (e *Executor) visionMaxBytes(ctx context.Context) int {
	if e.Settings != nil {
		if v, err := e.Settings.Int(ctx, settings.KeyToolReviewScreenshotMaxBytes); err == nil {
			return v
		}
	}
	return visionMaxBytesDefault
}

// visionThinkingLevel decides how hard the vision model thinks. The operator
// can pin it (google.vision_thinking_level); "auto", the default, lets the
// tool choose. Locating is a perception task and gets low; answering a
// question about a page is reasoning and gets medium. The upstream CLIs have
// no equivalent knob — VISION_REASONING_EFFORT is passed straight through to
// the provider and unset by default — so this is our default, applied where
// they would have sent none.
func (e *Executor) visionThinkingLevel(ctx context.Context, fallback string) string {
	if e.Settings != nil {
		if v, err := e.Settings.String(ctx, settings.KeyGoogleVisionThinkingLevel); err == nil && v != "" && v != "auto" {
			return v
		}
	}
	return fallback
}

// ---------------------------------------------------------------------------
// Glance
// ---------------------------------------------------------------------------

type glanceArgs struct {
	ImagePaths     []string `json:"image_paths"`
	Query          string   `json:"query"`
	OCR            bool     `json:"ocr"`
	OCRExtra       string   `json:"ocr_extra"`
	Region         string   `json:"region"`
	ConversationID string   `json:"conversation_id"`
}

// glancePrompt is bin/glance's build_prompt, ported branch for branch. The
// three cases are mutually exclusive upstream (argparse puts --query and
// --ocr in a mutually exclusive group), and the default prompt for a single
// image is vision_client.py's DEFAULT_PROMPT.
func glancePrompt(args glanceArgs, count int) string {
	if args.OCR {
		scope := "this image"
		if count > 1 {
			scope = "these images"
		}
		prompt := fmt.Sprintf("Transcribe every piece of visible text in %s verbatim (titles, body text, labels, watermarks, etc.), "+
			"line by line, without omitting any characters. Do not rewrite, summarize, or translate the text, "+
			"and do not add any preamble, explanation, or extra content.", scope)
		if count > 1 {
			prompt += " Label each image's text with its ordinal (Image 1, Image 2, ...)."
		}
		if args.OCRExtra != "" {
			prompt += fmt.Sprintf(" Additional requirements: %s", args.OCRExtra)
		}
		return prompt
	}
	if args.Query != "" {
		return args.Query
	}
	if count > 1 {
		return "Describe each image in detail (label them Image 1, Image 2, ...), " +
			"then point out the notable differences between them."
	}
	return "Please describe the contents of this image in detail."
}

// execGlance implements Glance: describe, answer a question about, or OCR one
// or more images. It is the general vision tool — the replacement for
// AskVision — and like AskVision it imposes no shape on the answer.
//
// Note what is no longer imposed: AskVision prepended a system instruction
// requiring a sentence on what was actually visible before the answer, which
// was measured to matter (docs/reviews/vision-path-2026-08-14.md — one
// session spent 23% of its cost establishing that a clean answer was clean).
// The upstream tool sends no such instruction and this port does not either.
// If that failure mode returns, this is the first place to look.
func execGlance(ctx context.Context, e *Executor, argsRaw json.RawMessage) Result {
	var args glanceArgs
	if err := json.Unmarshal(argsRaw, &args); err != nil {
		return errorResult("invalid arguments: %v", err)
	}
	var conversation *glanceConversation
	if args.ConversationID != "" {
		conversation = e.glanceConversation(args.ConversationID)
		if conversation == nil {
			// Naming the ones that exist rather than only the miss: an id
			// from a conversation this session dropped past the cap reads
			// identically to a typo, and the model can only act on the
			// difference if it is told.
			return errorResult("no Glance conversation %q in this session%s", args.ConversationID, glanceConversationHint(e))
		}
	}
	// image_paths is required on a first call and optional on a follow-up,
	// where omitting it means "the same images" and passing new ones replaces
	// them for the rest of the thread.
	imagesReplaced := conversation != nil && len(args.ImagePaths) > 0
	if len(args.ImagePaths) == 0 {
		if conversation == nil {
			return errorResult("image_paths is required and must name at least one file")
		}
		args.ImagePaths = conversation.imagePaths
	}
	if args.OCR && args.Query != "" {
		return errorResult("query and ocr are mutually exclusive: ocr transcribes the text, query asks a question")
	}
	if args.Region != "" && len(args.ImagePaths) > 1 {
		return errorResult("region works with exactly one image, got %d", len(args.ImagePaths))
	}
	maxImages := e.visionMaxImages(ctx)
	if len(args.ImagePaths) > maxImages {
		return errorResult("Glance accepts at most %d images, got %d", maxImages, len(args.ImagePaths))
	}

	var images []gemini.Image
	var notes []string
	var err error
	if args.Region != "" {
		var img gemini.Image
		var note string
		img, _, note, err = loadCroppedVisionImage(ctx, e, args.ImagePaths[0], args.Region)
		images = []gemini.Image{img}
		if note != "" {
			notes = append(notes, note)
		}
	} else {
		images, notes, err = loadVisionImages(ctx, e, args.ImagePaths)
	}
	if err != nil {
		return errorResult("%v", err)
	}
	if e.Gemini == nil {
		return errorResult("Glance is not available in this context: no Gemini client configured")
	}

	question := glancePrompt(args, len(images))
	sent := question
	if conversation != nil {
		sent = glanceFollowUpPrompt(conversation, question, imagesReplaced)
	}
	answer, usage, sentAt, err := visionInteract(ctx, e, "", sent, images,
		e.visionThinkingLevel(ctx, gemini.ThinkingLevelMedium))
	if err != nil {
		return errorResult("%v", err)
	}

	// The thread records the question as asked, not as sent: replaying a
	// follow-up's composed prompt into the next follow-up would nest the
	// history inside itself and grow every turn quadratically.
	turn := glanceTurn{question: question, answer: answer}
	conversationID := args.ConversationID
	if conversation == nil {
		conversationID = e.startGlanceConversation(args.ImagePaths, turn)
	} else {
		e.appendGlanceTurn(conversationID, args.ImagePaths, turn)
	}

	// Plain byte truncation is safe: there is no JSON document to sever, so a
	// cut answer is a short answer rather than a broken one.
	out, truncated := truncate(answer, e.outputCap(ctx))
	content := out + fmt.Sprintf("\n\nconversation_id: %s (pass it to ask a follow-up about these images without re-sending the thread)", conversationID)
	if len(notes) > 0 {
		// Ahead of the answer, so the model knows the vision model saw less
		// detail than the file holds before it reads what it concluded.
		content = strings.Join(notes, "\n") + "\n\n" + content
	}
	return visionResult(e, content, truncated, usage, sentAt)
}

// ---------------------------------------------------------------------------
// Ground and Detect
// ---------------------------------------------------------------------------

type groundArgs struct {
	ImagePath string `json:"image_path"`
	Target    string `json:"target"`
	Region    string `json:"region"`
}

type detectArgs struct {
	ImagePath string `json:"image_path"`
	Category  string `json:"category"`
	Region    string `json:"region"`
}

// detectDefaultCategory is detect.py's DEFAULT_CATEGORY, verbatim.
const detectDefaultCategory = "UI element (buttons, links, inputs, icons, labels, " +
	"headings, images, badges)"

// groundPrompt is ground.py's build_prompt, verbatim. The 0-1000 grid and the
// [y0, x0, y1, x1] ordering are Gemini's own bounding-box convention, which is
// why a prompt this thin works at all — and why the coordinates are
// resolution-independent, so downscaling an image before sending it does not
// move the boxes.
func groundPrompt(target string) string {
	return "Locate every visible object or region matching this target:\n" +
		target + "\n\n" +
		`Return only a JSON array. Each item must contain "box_2d" as ` +
		`[y0, x0, y1, x1] on a 0-1000 grid and "label" as a short description. ` +
		"Use tight boxes in the original image. Return [] when nothing matches."
}

// detectTarget is detect.py's build_target: Detect is Ground with a canned
// target and nothing else.
func detectTarget(category string) string {
	if category == "" {
		category = detectDefaultCategory
	}
	return fmt.Sprintf("every distinct %s — include the exact visible text in each label", category)
}

// visionMatch is one located thing: a label and a pixel box in the ORIGINAL
// image's coordinate space.
type visionMatch struct {
	Label string
	Box   [4]int // x1, y1, x2, y2
}

func execGround(ctx context.Context, e *Executor, argsRaw json.RawMessage) Result {
	var args groundArgs
	if err := json.Unmarshal(argsRaw, &args); err != nil {
		return errorResult("invalid arguments: %v", err)
	}
	if args.Target == "" {
		return errorResult("target is required: say what to locate")
	}
	return runLocate(ctx, e, "Ground", args.ImagePath, args.Target, args.Region)
}

func execDetect(ctx context.Context, e *Executor, argsRaw json.RawMessage) Result {
	var args detectArgs
	if err := json.Unmarshal(argsRaw, &args); err != nil {
		return errorResult("invalid arguments: %v", err)
	}
	return runLocate(ctx, e, "Detect", args.ImagePath, detectTarget(args.Category), args.Region)
}

// runLocate is ground.py's locate() plus its output formatting. Both tools
// share it because upstream Detect is a one-line wrapper around Ground.
func runLocate(ctx context.Context, e *Executor, tool, userPath, target, region string) Result {
	if userPath == "" {
		return errorResult("image_path is required")
	}
	path, err := resolveImagePath(e.Workspace, userPath)
	if err != nil {
		return errorResult("%v", err)
	}
	width, height, err := imageDimensions(path)
	if err != nil {
		return errorResult("%v", err)
	}

	var img gemini.Image
	// sentW/sentH are the dimensions the coordinates come back relative to:
	// the crop when a region was given, the whole image otherwise. Because the
	// grid is normalised these are the ONLY dimensions that matter for the
	// scaling — a downscale between here and the wire changes nothing.
	sentW, sentH := width, height
	var offset [2]int
	var notes []string
	if region != "" {
		var box [4]int
		var note string
		img, box, note, err = loadCroppedVisionImage(ctx, e, userPath, region)
		if err != nil {
			return errorResult("%v", err)
		}
		sentW, sentH = box[2]-box[0], box[3]-box[1]
		offset = [2]int{box[0], box[1]}
		if note != "" {
			notes = append(notes, note)
		}
	} else {
		images, downscaled, loadErr := loadVisionImages(ctx, e, []string{userPath})
		if loadErr != nil {
			return errorResult("%v", loadErr)
		}
		img = images[0]
		notes = downscaled
	}
	if e.Gemini == nil {
		return errorResult("%s is not available in this context: no Gemini client configured", tool)
	}

	// 8192 is upstream's, with upstream's reason: an exhaustive target on a
	// dense screen emits dozens of boxes, and 2048 truncated the JSON
	// mid-array (ground.py:157-159). Locating is perception rather than
	// reasoning, so the thinking default is low.
	answer, usage, sentAt, err := visionInteract(ctx, e, "", groundPrompt(target), []gemini.Image{img},
		e.visionThinkingLevel(ctx, gemini.ThinkingLevelLow))
	if err != nil {
		return errorResult("%v", err)
	}

	matches, parseErr := parseMatches(answer, sentW, sentH, target)
	if parseErr != nil {
		// The usage still rides home: the call was made and billed whatever
		// the answer turned out to be.
		return visionResult(e, fmt.Sprintf("%v\n\nThe model replied:\n%s", parseErr, answer), false, usage, sentAt)
	}
	// Matches were parsed in the sent image's coordinates; report them in the
	// original image's (ground.py:163-165).
	for i := range matches {
		matches[i].Box = [4]int{
			matches[i].Box[0] + offset[0], matches[i].Box[1] + offset[1],
			matches[i].Box[2] + offset[0], matches[i].Box[3] + offset[1],
		}
	}

	var lines []string
	if tool == "Detect" {
		lines = formatInventory(matches, width, height)
	} else {
		lines = formatMatches(matches, width, height)
	}
	// The downscale note matters more here than it does for Glance, not less.
	// The boxes are still in the original image's pixels — the grid is
	// normalised, so the arithmetic survives a downscale untouched — but the
	// model's ability to SEE a small target does not, and a locate that
	// returns nothing on a shrunk capture reads identically to one that
	// returned nothing because the target is absent. Ahead of the boxes, so
	// an empty answer arrives with the reason it might be empty.
	body := strings.Join(lines, "\n")
	if len(notes) > 0 {
		body = strings.Join(notes, "\n") + "\n\n" + body
	}
	out, truncated := truncate(body, e.outputCap(ctx))
	return visionResult(e, out, truncated, usage, sentAt)
}

// boxKeys are the key spellings upstream accepts for a bounding box, in
// preference order (ground.py:87-92). A model that answers in any of them is
// answering correctly enough.
var boxKeys = []string{"box_2d", "bbox_2d", "box2d", "bbox", "box"}

// wrapperKeys are the object keys upstream looks under when the reply is a
// JSON object rather than the requested array (ground.py:80).
var wrapperKeys = []string{"boxes", "bounding_boxes", "bboxes", "objects", "items", "results"}

var (
	// The scavenger: one {...} block carrying a box-ish key with an array.
	fallbackObjectRE = regexp.MustCompile(`\{[^{}]*['"](?:box_2d|bbox_2d|box2d|bbox|box)['"]\s*:\s*\[[^\]]+\][^{}]*\}`)
	fallbackBoxRE    = regexp.MustCompile(`['"](?:box_2d|bbox_2d|box2d|bbox|box)['"]\s*:\s*\[([^\]]+)\]`)
	fallbackLabelRE  = regexp.MustCompile(`['"](?:label|caption|description)['"]\s*:\s*['"]([^'"]+)['"]`)
	numberRE         = regexp.MustCompile(`-?\d+(?:\.\d+)?`)
)

// parseMatches is ground.py's _items + _normalize_box + parse_matches. The
// tolerance is the product: the model does sometimes wrap the array in prose
// or in an object, and a strict parser turns a usable answer into a failure.
func parseMatches(answer string, width, height int, target string) ([]visionMatch, error) {
	items, err := groundItems(answer)
	if err != nil {
		return nil, err
	}
	matches := make([]visionMatch, 0, len(items))
	for _, item := range items {
		box, ok := normalizeBox(item, width, height)
		if !ok {
			continue
		}
		label := firstString(item, "label", "caption", "description")
		if label == "" {
			label = target
		}
		matches = append(matches, visionMatch{Label: label, Box: box})
	}
	return matches, nil
}

// groundItems recovers the array of box objects from the model's reply:
// fenced JSON first, then a bare array, then an object wrapping one, then the
// regex scavenger.
func groundItems(answer string) ([]map[string]any, error) {
	cleaned := stripJSONFence(answer)

	var asArray []map[string]any
	if err := json.Unmarshal([]byte(cleaned), &asArray); err == nil {
		return asArray, nil
	}
	var asObject map[string]json.RawMessage
	if err := json.Unmarshal([]byte(cleaned), &asObject); err == nil {
		for _, key := range wrapperKeys {
			raw, ok := asObject[key]
			if !ok {
				continue
			}
			var items []map[string]any
			if err := json.Unmarshal(raw, &items); err == nil {
				return items, nil
			}
		}
		return nil, fmt.Errorf("the vision model returned an incompatible bounding-box JSON structure")
	}
	if items := scavengeItems(cleaned); len(items) > 0 {
		return items, nil
	}
	return nil, fmt.Errorf("the vision model did not return parseable bounding-box JSON")
}

// scavengeItems is _fallback_items: pull box objects out of prose with
// regexes when the document as a whole will not parse.
func scavengeItems(text string) []map[string]any {
	var items []map[string]any
	for _, block := range fallbackObjectRE.FindAllString(text, -1) {
		boxMatch := fallbackBoxRE.FindStringSubmatch(block)
		if boxMatch == nil {
			continue
		}
		numbers := numberRE.FindAllString(boxMatch[1], -1)
		if len(numbers) < 4 {
			continue
		}
		values := make([]any, 0, 4)
		for _, n := range numbers[:4] {
			f, err := strconv.ParseFloat(n, 64)
			if err != nil {
				break
			}
			values = append(values, f)
		}
		if len(values) < 4 {
			continue
		}
		item := map[string]any{"box_2d": values}
		if labelMatch := fallbackLabelRE.FindStringSubmatch(block); labelMatch != nil {
			item["label"] = strings.TrimSpace(labelMatch[1])
		}
		items = append(items, item)
	}
	return items
}

// normalizeBox is _normalize_box: find the box under any accepted key, swap
// inverted axes, scale off the 0-1000 grid, clamp to the image, and drop
// anything degenerate.
func normalizeBox(item map[string]any, width, height int) ([4]int, bool) {
	var raw []any
	for _, key := range boxKeys {
		if v, ok := item[key].([]any); ok && len(v) == 4 {
			raw = v
			break
		}
	}
	if raw == nil {
		return [4]int{}, false
	}
	nums := make([]float64, 4)
	for i, v := range raw {
		f, ok := toFloat(v)
		if !ok {
			return [4]int{}, false
		}
		nums[i] = f
	}
	y0, x0, y1, x1 := nums[0], nums[1], nums[2], nums[3]
	if x0 > x1 {
		x0, x1 = x1, x0
	}
	if y0 > y1 {
		y0, y1 = y1, y0
	}
	box := [4]int{
		clampInt(int(math.Round(x0/1000*float64(width))), 0, width),
		clampInt(int(math.Round(y0/1000*float64(height))), 0, height),
		clampInt(int(math.Round(x1/1000*float64(width))), 0, width),
		clampInt(int(math.Round(y1/1000*float64(height))), 0, height),
	}
	if box[2] <= box[0] || box[3] <= box[1] {
		return [4]int{}, false
	}
	return box, true
}

// formatMatches is ground.py's format_matches: a single match prints its box
// bare, several print numbered with a coarse position word.
func formatMatches(matches []visionMatch, width, height int) []string {
	if len(matches) == 0 {
		return []string{"no match found"}
	}
	if len(matches) == 1 {
		b := matches[0].Box
		return []string{fmt.Sprintf("x1: %d, y1: %d, x2: %d, y2: %d", b[0], b[1], b[2], b[3])}
	}
	return numberedMatches(matches, width, height)
}

// formatInventory is detect.py's format_inventory: always numbered, and a
// different empty message.
func formatInventory(matches []visionMatch, width, height int) []string {
	if len(matches) == 0 {
		return []string{"no elements detected"}
	}
	return numberedMatches(matches, width, height)
}

func numberedMatches(matches []visionMatch, width, height int) []string {
	lines := make([]string, 0, len(matches))
	for i, m := range matches {
		lines = append(lines, fmt.Sprintf("%d. %s %s x1: %d, y1: %d, x2: %d, y2: %d",
			i+1, boxPosition(m.Box, width, height), m.Label, m.Box[0], m.Box[1], m.Box[2], m.Box[3]))
	}
	return lines
}

// boxPosition is ground.py's _position: which ninth of the image the box's
// centre falls in.
func boxPosition(box [4]int, width, height int) string {
	x := float64(box[0]+box[2]) / 2
	y := float64(box[1]+box[3]) / 2
	horizontal := "center"
	switch {
	case x < float64(width)/3:
		horizontal = "left"
	case x > float64(width)*2/3:
		horizontal = "right"
	}
	vertical := "center"
	switch {
	case y < float64(height)/3:
		vertical = "top"
	case y > float64(height)*2/3:
		vertical = "bottom"
	}
	switch {
	case vertical == "center" && horizontal == "center":
		return "center"
	case vertical == "center":
		return horizontal
	case horizontal == "center":
		return vertical
	default:
		return vertical + "-" + horizontal
	}
}

// ---------------------------------------------------------------------------
// Crop
// ---------------------------------------------------------------------------

type cropArgs struct {
	ImagePath string `json:"image_path"`
	Region    string `json:"region"`
	Output    string `json:"output"`
	Scale     int    `json:"scale"`
}

// execCrop is bin/crop: cut a pixel box out of an image into its own file,
// optionally upscaled. No model call, no cost — it is here so a box from
// Ground can become an image Glance reads at full resolution.
func execCrop(ctx context.Context, e *Executor, argsRaw json.RawMessage) Result {
	var args cropArgs
	if err := json.Unmarshal(argsRaw, &args); err != nil {
		return errorResult("invalid arguments: %v", err)
	}
	if args.ImagePath == "" {
		return errorResult("image_path is required")
	}
	if args.Region == "" {
		return errorResult("region is required: pass the pixel box to cut out, as X1,Y1,X2,Y2")
	}
	// 0 is the absent value, not a rejected one: JSON has no way to say "the
	// caller did not pass scale", so an omitted scale and scale: 0 arrive
	// identically and both mean no upscale. Anything above 8 is refused
	// because the upscale is memory the crop does not need — 8x a 500x500 box
	// is 16 megapixels.
	if args.Scale < 0 || args.Scale > 8 {
		return errorResult("scale must be between 1 and 8, got %d (omit it for no upscale)", args.Scale)
	}
	path, err := resolveImagePath(e.Workspace, args.ImagePath)
	if err != nil {
		return errorResult("%v", err)
	}
	src, mimeType, err := decodeVisionImage(path)
	if err != nil {
		return errorResult("%v", err)
	}
	bounds := src.Bounds()
	box, err := parseRegion(args.Region, bounds.Dx(), bounds.Dy())
	if err != nil {
		return errorResult("%v", err)
	}

	out := args.Output
	if out == "" {
		// Upstream writes <image-stem>.crop.png beside the input, which is
		// right for a CLI on your own machine and wrong here: beside the input
		// can be inside a cloned repository, where the crop turns up in that
		// repository's diff. The base name alone is relative, so the scratch
		// rule re-roots it under scratch/ — the same place Screenshot puts a
		// capture, and what makes Crop safe to allow in read-only mode
		// (policy.go).
		base := filepath.Base(args.ImagePath)
		out = strings.TrimSuffix(base, filepath.Ext(base)) + ".crop.png"
	}
	outPath, err := resolveScratchImageOutput(e.Workspace, out)
	if err != nil {
		return errorResult("%v", err)
	}

	// scratch/ exists in a prepared workspace but a
	// nested output path under it may not, and a crop that resolves fine and
	// then fails on the write is a confusing refusal.
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return errorResult("create the directory for %s: %v", out, err)
	}

	cropped := cropAndScale(src, box, args.Scale)
	encoded, encodedMIME, err := encodeScreenshot(cropped, cropOutputMIME(outPath, mimeType))
	if err != nil {
		return errorResult("encode crop: %v", err)
	}
	if err := os.WriteFile(outPath, encoded, 0o644); err != nil {
		return errorResult("write %s: %v", out, err)
	}
	size := cropped.Bounds()
	line := fmt.Sprintf("Wrote %s: %dx%d %s, %d bytes, cut from %s at x1: %d, y1: %d, x2: %d, y2: %d",
		out, size.Dx(), size.Dy(), strings.TrimPrefix(encodedMIME, "image/"), len(encoded),
		args.ImagePath, box[0], box[1], box[2], box[3])
	if args.Scale > 1 {
		line += fmt.Sprintf(", upscaled %dx", args.Scale)
	}
	return Result{Content: line}
}

// cropAndScale cuts box out of src and, when scale is above 1, enlarges it.
// The standard library has no scaler; CatmullRom is the same resampler the
// downscale path uses, standing in for upstream's LANCZOS.
func cropAndScale(src image.Image, box [4]int, scale int) image.Image {
	rect := image.Rect(box[0], box[1], box[2], box[3]).Add(src.Bounds().Min)
	dst := image.NewRGBA(image.Rect(0, 0, rect.Dx(), rect.Dy()))
	draw.Draw(dst, dst.Bounds(), src, rect.Min, draw.Src)
	if scale <= 1 {
		return dst
	}
	scaled := image.NewRGBA(image.Rect(0, 0, rect.Dx()*scale, rect.Dy()*scale))
	draw.CatmullRom.Scale(scaled, scaled.Bounds(), dst, dst.Bounds(), draw.Over, nil)
	return scaled
}

// cropOutputMIME picks the encoding from the output path's extension, falling
// back to the source's when the extension names nothing we encode.
func cropOutputMIME(outPath, sourceMIME string) string {
	if mimeType, ok := attachment.MIMEType(outPath); ok {
		return mimeType
	}
	return sourceMIME
}

// ---------------------------------------------------------------------------
// Shared plumbing
// ---------------------------------------------------------------------------

// visionInteract makes the call and reports the instant it went out, which is
// what the price table costs against (internal/pricing.Table.Cost).
func visionInteract(ctx context.Context, e *Executor, instruction, prompt string, images []gemini.Image, thinking string) (string, *gemini.Usage, time.Time, error) {
	model := gemini.DefaultModel
	if e.GeminiModel != nil {
		m, err := e.GeminiModel()
		if err != nil {
			return "", nil, time.Time{}, fmt.Errorf("resolve vision model: %v", err)
		}
		if m != "" {
			model = m
		}
	}
	sentAt := time.Now()
	answer, usage, err := e.Gemini.Interact(ctx, model, instruction, prompt, images,
		gemini.WithThinkingLevel(thinking),
		// The field carries a type and no schema, so constraining the
		// container empties or mangles the contents — measured, and the reason
		// Ground parses tolerantly instead of asking for structured output
		// (docs/VISION-TOOLKIT.md §6).
		gemini.WithResponseFormat(""))
	if err != nil {
		return "", nil, sentAt, err
	}
	return answer, usage, sentAt, nil
}

// visionResult attaches the usage payload and the cost line. The usage rides
// on the result so the vision spend is a separable event in the log, and the
// figure comes home in the text because the tool description cannot carry a
// number (docs/CACHE.md) and "expensive" on its own has not stopped a session
// spending a third of its budget here.
func visionResult(e *Executor, content string, truncated bool, usage *gemini.Usage, sentAt time.Time) Result {
	res := Result{Content: content, Truncated: truncated}
	if usage != nil {
		model := gemini.DefaultModel
		if e.GeminiModel != nil {
			if m, err := e.GeminiModel(); err == nil && m != "" {
				model = m
			}
		}
		res.GeminiUsage = geminiUsagePayload(e.Prices, model, sentAt, usage)
		if line := visionCostLine(res.GeminiUsage, model); line != "" {
			res.Content += "\n\n" + line
		}
	}
	return res
}

func visionCostLine(usage *store.UsagePayload, model string) string {
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

// parseRegion is the X1,Y1,X2,Y2 argument the CLIs share, clamped to the
// image the way _parse_region clamps it.
func parseRegion(region string, width, height int) ([4]int, error) {
	parts := strings.Split(region, ",")
	if len(parts) != 4 {
		return [4]int{}, fmt.Errorf("region expects four integers: X1,Y1,X2,Y2 (pixels), got %q", region)
	}
	var v [4]int
	for i, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			return [4]int{}, fmt.Errorf("region expects four integers: X1,Y1,X2,Y2 (pixels), got %q", region)
		}
		v[i] = n
	}
	box := [4]int{
		clampInt(minInt(v[0], v[2]), 0, width),
		clampInt(minInt(v[1], v[3]), 0, height),
		clampInt(maxInt(v[0], v[2]), 0, width),
		clampInt(maxInt(v[1], v[3]), 0, height),
	}
	if box[2] <= box[0] || box[3] <= box[1] {
		return [4]int{}, fmt.Errorf("region %s is empty after clamping to %dx%d", region, width, height)
	}
	return box, nil
}

// imageDimensions reads an image's pixel size without decoding the whole
// file. Ground scales its normalised boxes against these, not against
// whatever dimensions survived the byte cap.
func imageDimensions(path string) (int, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, fmt.Errorf("read %s: %v", filepath.Base(path), err)
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return 0, 0, fmt.Errorf("decode %s: %v", filepath.Base(path), err)
	}
	return cfg.Width, cfg.Height, nil
}

// decodeVisionImage resolves, validates and decodes one image file.
func decodeVisionImage(path string) (image.Image, string, error) {
	mimeType, ok := attachment.MIMEType(path)
	if !ok {
		return nil, "", fmt.Errorf("unsupported image type for %s: PNG, JPEG, and WebP are accepted", filepath.Base(path))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read %s: %v", filepath.Base(path), err)
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("decode %s: %v", filepath.Base(path), err)
	}
	return img, mimeType, nil
}

// loadCroppedVisionImage crops one image to region and returns it ready to
// send, with the box it cut in the original image's coordinates. Upstream
// sends the crop and nothing else, which is the point: a small control fills
// the frame instead of being a few pixels of a page.
// The third return value is the downscale note, empty when the crop fitted
// the byte cap as cut — the same note loadVisionImages produces, for the same
// reason: the caller has to be able to tell the model that what the vision
// model saw held less detail than the file does.
func loadCroppedVisionImage(ctx context.Context, e *Executor, userPath, region string) (gemini.Image, [4]int, string, error) {
	path, err := resolveImagePath(e.Workspace, userPath)
	if err != nil {
		return gemini.Image{}, [4]int{}, "", err
	}
	src, mimeType, err := decodeVisionImage(path)
	if err != nil {
		return gemini.Image{}, [4]int{}, "", err
	}
	bounds := src.Bounds()
	box, err := parseRegion(region, bounds.Dx(), bounds.Dy())
	if err != nil {
		return gemini.Image{}, [4]int{}, "", err
	}
	encoded, encodedMIME, err := encodeScreenshot(cropAndScale(src, box, 1), mimeType)
	if err != nil {
		return gemini.Image{}, [4]int{}, "", fmt.Errorf("encode the crop of %s: %v", userPath, err)
	}
	var note string
	maxBytes := int64(e.visionMaxBytes(ctx))
	if int64(len(encoded)) > maxBytes {
		shrunk, shrunkMIME, w, h, err := downscaleImage(encoded, encodedMIME, maxBytes)
		if err != nil {
			return gemini.Image{}, [4]int{}, "", fmt.Errorf("the crop of %s is %d bytes, over the %d-byte per-file limit, and could not be downscaled: %v", userPath, len(encoded), maxBytes, err)
		}
		note = fmt.Sprintf("the %s crop of %s was downscaled to %dx%d to fit the %d-byte per-file limit", region, userPath, w, h, maxBytes)
		encoded, encodedMIME = shrunk, shrunkMIME
	}
	return gemini.Image{
		Data:       encoded,
		MIMEType:   encodedMIME,
		Resolution: gemini.ResolutionHigh,
		Label:      filepath.Base(path),
	}, box, note, nil
}

// loadVisionImages reads and validates the image files paths names. The first
// image goes at high resolution and the rest at medium, and each image's
// label is its file's base name, so an answer about several images can say
// which one it means.
//
// A file over the byte cap is downscaled rather than refused: it is decoded,
// shrunk preserving aspect ratio until the re-encoded bytes fit under the cap,
// and the shrunk bytes are sent. The second return value carries one note per
// downscaled file — which file, and to what dimensions — so the model knows
// the vision model saw less detail than the file holds. A file that will not
// decode keeps the refusal, naming the limit and the reason. A file already
// under the cap is sent byte-identically: it is never decoded and re-encoded.
func loadVisionImages(ctx context.Context, e *Executor, paths []string) ([]gemini.Image, []string, error) {
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
			return nil, nil, fmt.Errorf("%s is a directory, not an image", userPath)
		}
		mimeType, ok := attachment.MIMEType(path)
		if !ok {
			return nil, nil, fmt.Errorf("unsupported image type for %s: PNG, JPEG, and WebP files are accepted", userPath)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, nil, fmt.Errorf("read %s: %v", userPath, err)
		}

		maxBytes := int64(e.visionMaxBytes(ctx))
		if int64(len(data)) > maxBytes {
			shrunk, shrunkMIME, w, h, err := downscaleImage(data, mimeType, maxBytes)
			if err != nil {
				// The refusal names both the limit and why the route around it
				// (downscaling) was not available, so the model knows a
				// re-capture is the only way forward rather than retrying the
				// same file.
				return nil, nil, fmt.Errorf("image %s is %d bytes, over the %d-byte per-file limit, and could not be downscaled: %v", userPath, len(data), maxBytes, err)
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
// PNG — the only re-encode that keeps alpha. An image that will not decode at
// all, or that still does not fit at the smallest size, is an error the caller
// turns into the refusal.
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

// stripJSONFence removes a ```json fence from a model reply, keeping what is
// inside it.
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

// geminiUsagePayload turns one successful Gemini call's usage into the store's
// usage-event shape, cost included, so the runner can commit it and the
// session's cost total picks it up like any DeepSeek turn. The token mapping
// (thinking billed at the output rate, cached input a subset of input) is
// decided and documented in gemini.Usage.TokenSplit. Cost is computed against
// the same price table DeepSeek's turns use, keyed by the vision model that
// actually ran. A nil price table, or a model with no entry, leaves cost at
// zero rather than failing the tool result: the run itself succeeded, and the
// operator's price table is the thing that is incomplete (docs/DESIGN.md §4.9
// prices load from config, never from code).
//
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
		// the only thing distinguishing a two-cent vision call from a sub-cent
		// DeepSeek turn in the log was the collision.
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

func firstString(item map[string]any, keys ...string) string {
	for _, key := range keys {
		if s, ok := item[key].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		return f, err == nil
	}
	return 0, false
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// Glance conversations
// ---------------------------------------------------------------------------

// maxGlanceConversations caps how many Glance conversations one session holds
// at once. Each is a handful of paths and text pairs, so the cap bounds a long
// session's growth rather than memory; the most recent are kept.
const maxGlanceConversations = 5

// glanceTurn is one exchange. The answer is Gemini's raw text rather than the
// formatted tool result, because the raw text is what gets re-sent.
type glanceTurn struct {
	question string
	answer   string
}

// glanceConversation is what a follow-up needs to continue without the caller
// restating the thread: the image paths (files are re-read from disk each
// time, so no bytes are held) and the exchanges so far.
//
// This is ReviewScreenshot's conversation_id, brought back onto Glance. It was
// lost in the port because the upstream CLI is stateless — every invocation is
// a fresh process — and a CLI has nowhere to keep a thread. A tool call does.
// Without it, "look closer at the header" re-uploads the image and re-derives
// context the model already had, and the earlier answer is gone unless the
// caller pastes it back.
//
// imagePaths is the one part a follow-up may replace, by passing its own
// image_paths — the re-capture loop the tool is used in.
type glanceConversation struct {
	imagePaths []string
	turns      []glanceTurn
}

func (e *Executor) glanceConversation(id string) *glanceConversation {
	e.glanceMu.Lock()
	defer e.glanceMu.Unlock()
	return e.glanceConversations[id]
}

// startGlanceConversation records a fresh conversation and returns its id. The
// map is created lazily so an Executor built without the field (every test
// that constructs one directly) works unchanged.
func (e *Executor) startGlanceConversation(imagePaths []string, turn glanceTurn) string {
	e.glanceMu.Lock()
	defer e.glanceMu.Unlock()
	if e.glanceConversations == nil {
		e.glanceConversations = make(map[string]*glanceConversation)
	}
	id := newGlanceConversationID()
	e.glanceConversations[id] = &glanceConversation{imagePaths: imagePaths, turns: []glanceTurn{turn}}
	e.glanceOrder = append(e.glanceOrder, id)
	if len(e.glanceOrder) > maxGlanceConversations {
		oldest := e.glanceOrder[0]
		e.glanceOrder = e.glanceOrder[1:]
		delete(e.glanceConversations, oldest)
	}
	return id
}

// appendGlanceTurn records one more exchange and re-points the conversation at
// the images that exchange was about, so the next follow-up continues from the
// newest capture rather than reverting to the ones it opened with.
func (e *Executor) appendGlanceTurn(id string, imagePaths []string, turn glanceTurn) {
	e.glanceMu.Lock()
	defer e.glanceMu.Unlock()
	if c := e.glanceConversations[id]; c != nil {
		c.imagePaths = imagePaths
		c.turns = append(c.turns, turn)
	}
}

// newGlanceConversationID mints an id. The prefix makes its origin obvious in
// a tool result the model echoes back.
func newGlanceConversationID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("tools: crypto/rand unavailable: " + err.Error())
	}
	return "gl-" + hex.EncodeToString(b[:])
}

// glanceFollowUpPrompt composes a follow-up: the earlier exchanges, then the
// new question last — data first, question last, the same shape a first call
// has. imagesReplaced says the images attached now are not the ones the thread
// describes, which the model has to be told: replayed beside its own earlier
// answer, a fresh capture otherwise gets reconciled against that answer
// instead of being looked at.
func glanceFollowUpPrompt(conversation *glanceConversation, prompt string, imagesReplaced bool) string {
	var b strings.Builder
	if len(conversation.turns) > 0 {
		b.WriteString("Earlier in this conversation:\n")
		for _, turn := range conversation.turns {
			fmt.Fprintf(&b, "Question: %s\nAnswer: %s\n\n", turn.question, turn.answer)
		}
	}
	if imagesReplaced {
		b.WriteString("The images attached to this message are NEW captures, taken after the exchange above. They replace the ones you were shown earlier. Judge what you can see now; treat the earlier exchange as history, not as a description of these images.\n\n")
	}
	b.WriteString(prompt)
	return b.String()
}

// glanceConversationHint names the conversations this session still holds, so
// a rejected id can be corrected rather than guessed at.
func glanceConversationHint(e *Executor) string {
	e.glanceMu.Lock()
	defer e.glanceMu.Unlock()
	if len(e.glanceOrder) == 0 {
		return "; none have been started yet"
	}
	return "; open conversations: " + strings.Join(e.glanceOrder, ", ")
}
