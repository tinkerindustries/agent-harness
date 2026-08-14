package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mrgeoffrich/deepseek-harness/internal/gemini"
	"github.com/mrgeoffrich/deepseek-harness/internal/settings"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

// Transcribe: chunked OCR of an image too tall for one vision call to read.
//
// The reason it exists is a fixed cost, not a byte cap. An image costs the
// vision model a flat ~1,120 input tokens at high resolution however large it
// is (docs/gemini-3.5-flash-ui-review-prompting.md, and 1,121-1,195 measured
// per image across a live run, docs/VISION-TOOLKIT.md §7). So a 1200x12000
// page capture gets the same visual budget as a 1200x800 viewport shot —
// roughly fifteen times less detail per unit of page — and Glance's answer
// about a tall capture describes the part it could still resolve. A live run
// noticed that unprompted: a "whole page" description covered the captured
// 800px viewport rather than the 2,640px document. Cutting the page into
// chunks and sending each one separately is not a workaround for a size
// limit; it is the only way to buy more effective resolution.
//
// The cut-band and merge logic is ported from Anionex/agent-vision-toolkit
// (MIT, © 2026 Anionex), skills/vision-tools/scripts/long_screenshot_ocr.py.
// Only those two ideas are ported. The upstream script is a Python
// orchestrator that shells out to a `glance` CLI, which cannot work here —
// vision is a tool the model calls, so there is no script in the loop to do
// the calling. Two deliberate simplifications against upstream:
//
//   - Upstream scores a cut with a blend of edge energy and foreground
//     occupancy over a downscaled analysis image. This measures ink alone
//     (pixels differing from the row's own background) at full row
//     resolution. Rows are the axis being cut on, and an edge-energy term
//     mostly restates what the ink count already says about a row of text.
//   - Upstream falls back to difflib.SequenceMatcher for a fuzzy line match
//     at a seam. This matches whole normalised lines and nothing else. A
//     ratio threshold on prose is the kind of knob that silently deletes a
//     real line that happens to resemble its neighbour, and a seam that
//     merged wrongly reads as clean output — see seamReport, which is why
//     an unmatched overlap is reported for review rather than guessed at.
//
// What is NOT simplified is the seam audit. Upstream ships one because a
// merge fails silently: a dropped or duplicated paragraph at a boundary
// reads as ordinary prose, and is worse than no tool at all. Every call
// reports every boundary, what was removed there, and which ones to check.

// Chunk geometry. None of these is a setting: they are the shape of the
// algorithm rather than operator policy, and an operator who wants a
// different chunk size wants a different tool.
const (
	// transcribeAnalysisColumns is roughly how many columns of each row are
	// sampled for its ink count. A full-width scan of a 1200x12000 capture
	// is 14 million pixel reads to answer a question about 12,000 rows;
	// sampling every Nth column answers it as well for a tenth of the work,
	// because a row of text puts ink under far more than one column in
	// thirty.
	transcribeAnalysisColumns = 400
	// transcribeInkThreshold is how far a pixel must sit from its row's
	// background, on its furthest channel, to count as ink. Upstream's
	// value: below it, JPEG ringing and subpixel antialiasing on a blank
	// row register as content and no row is ever blank.
	transcribeInkThreshold = 14
	// transcribeCleanMarginPx is how many blank rows either side of a cut
	// make it safe. It is small on purpose: a glyph that crossed the cut
	// would have put ink on those rows, so any genuine blank band is enough
	// and the ordinary leading between two lines of text — under ten pixels
	// on a normal page — is a perfectly good place to cut. A few pixels
	// rather than one guards against a row of antialiasing fringe reading as
	// blank. Setting this to the width of the overlap below was the first
	// attempt and it was wrong: it declared every ordinary line gap unsafe,
	// so every seam sent overlap and every seam came back flagged.
	transcribeCleanMarginPx = 4
	// transcribeOverlapPx is how much of the neighbouring chunk each side of
	// a boundary sees when the cut could NOT be placed in clean space — a
	// dense table, a terminal capture, anything with no gap to aim at. A
	// clean cut gets no overlap at all: there is nothing at the boundary to
	// duplicate, so duplicating it only gives the merge a chance to delete
	// something real.
	transcribeOverlapPx = 48
	// transcribeWidestBandPx is where a wider blank band stops being a
	// better place to cut. Past it the cut is as clean as it can be and the
	// only thing left to prefer is proximity to the target height.
	transcribeWidestBandPx = 24
	// transcribeMaxOverlapLines bounds the run of lines the merge will
	// consider a repeat. Upstream's value; a seam repeats a line or two of
	// context, never a screenful.
	transcribeMaxOverlapLines = 24
)

// Bounds on the fan-out. Both are settings because both are about what the
// operator's API quota and the harness's timeout budget will bear, not about
// where a page should be cut.
const (
	transcribeMaxChunksDefault   = 24
	transcribeConcurrencyDefault = 4
)

func (e *Executor) transcribeMaxChunks(ctx context.Context) int {
	if e.Settings != nil {
		if v, err := e.Settings.Int(ctx, settings.KeyToolTranscribeMaxChunks); err == nil {
			return v
		}
	}
	return transcribeMaxChunksDefault
}

func (e *Executor) transcribeConcurrency(ctx context.Context) int {
	if e.Settings != nil {
		if v, err := e.Settings.Int(ctx, settings.KeyToolTranscribeConcurrency); err == nil {
			return v
		}
	}
	return transcribeConcurrencyDefault
}

type transcribeArgs struct {
	ImagePath string `json:"image_path"`
	Region    string `json:"region"`
}

// ---------------------------------------------------------------------------
// The tool
// ---------------------------------------------------------------------------

func execTranscribe(ctx context.Context, e *Executor, argsRaw json.RawMessage) Result {
	var args transcribeArgs
	if err := json.Unmarshal(argsRaw, &args); err != nil {
		return errorResult("invalid arguments: %v", err)
	}
	if args.ImagePath == "" {
		return errorResult("image_path is required")
	}
	path, err := resolveImagePath(e.Workspace, args.ImagePath)
	if err != nil {
		return errorResult("%v", err)
	}
	src, mimeType, err := decodeVisionImage(path)
	if err != nil {
		return errorResult("%v", err)
	}

	// A region is honoured by cutting the source down to it first, so
	// everything below — the ink profile, the cut search, the chunk
	// geometry — works on exactly the pixels being transcribed. offset is
	// what puts the reported cut positions back into the original image's
	// coordinates, which is the only frame the caller can act on: a y from
	// this report has to be a y it can hand to Crop.
	bounds := src.Bounds()
	offset := 0
	if args.Region != "" {
		box, err := parseRegion(args.Region, bounds.Dx(), bounds.Dy())
		if err != nil {
			return errorResult("%v", err)
		}
		src = cropAndScale(src, box, 1)
		bounds = src.Bounds()
		offset = box[1]
	}
	width, height := bounds.Dx(), bounds.Dy()

	plan := planChunks(src, e.transcribeMaxChunks(ctx))
	if len(plan) == 0 {
		return errorResult("%s has no pixels to transcribe", args.ImagePath)
	}
	if e.Gemini == nil {
		return errorResult("Transcribe is not available in this context: no Gemini client configured")
	}
	if over := len(plan) - e.transcribeMaxChunks(ctx); over > 0 {
		// Unreachable while planChunks honours the same cap; kept because
		// the cap is the thing standing between a mis-measured image and
		// dozens of billed calls, and a silent overrun of it is the failure
		// worth being loud about.
		return errorResult("%s would need %d chunks, over the %d-chunk limit; pass region to transcribe part of it",
			args.ImagePath, len(plan), e.transcribeMaxChunks(ctx))
	}

	images, notes, err := encodeChunks(ctx, e, src, plan, mimeType, args.ImagePath)
	if err != nil {
		return errorResult("%v", err)
	}

	results := e.runTranscribeChunks(ctx, plan, images)

	// Usage first, and unconditionally: every chunk that came back from the
	// API was billed whether or not its neighbours were, and whether or not
	// the merge below ever runs. Anything that returns from here carries it.
	usage := sumTranscribeUsage(results)

	var failed []string
	for _, r := range results {
		if r.err != nil {
			failed = append(failed, fmt.Sprintf("chunk %d (y %d-%d): %v",
				r.chunk.index+1, r.chunk.top+offset, r.chunk.bottom+offset, r.err))
		}
	}
	if len(failed) > 0 {
		// No partial transcript. A transcription with one chunk's worth of
		// the page missing from the middle reads as a complete document —
		// exactly the silent hole this tool's seam audit exists to prevent,
		// and it would be perverse to guard the boundaries and then ship a
		// gap. The successful chunks' cost still rides home below.
		content := fmt.Sprintf("%d of %d chunks failed, so no transcript was assembled — a partial one would read as a whole page with a hole in it:\n%s",
			len(failed), len(plan), strings.Join(failed, "\n"))
		res := Result{Content: content, IsError: true}
		attachTranscribeUsage(&res, usage)
		return res
	}

	merged, seams := mergeTranscripts(results)
	report := transcribeReport(args, width, height, offset, plan, seams, notes)

	// The report goes ahead of the transcript rather than after it, because
	// the transcript is the part that gets truncated. A seam audit that the
	// output cap can cut off is not an audit — and the boundaries to check
	// are what the caller needs to read before it starts trusting the text,
	// not after.
	budget := e.outputCap(ctx) - len(report) - transcribeCostLineReserve
	if budget < transcribeMinTranscriptBudget {
		budget = transcribeMinTranscriptBudget
	}
	body, truncated := truncate(merged, budget)

	res := Result{Content: report + "\n\n" + body, Truncated: truncated}
	attachTranscribeUsage(&res, usage)
	return res
}

const (
	// transcribeCostLineReserve holds back enough of the output cap for the
	// cost line visionResult appends after truncation has already happened.
	transcribeCostLineReserve = 200
	// transcribeMinTranscriptBudget is the floor under the transcript's
	// share of the cap. An operator who sets a tiny output cap gets a
	// truncated transcript rather than an empty one.
	transcribeMinTranscriptBudget = 1000
)

// ---------------------------------------------------------------------------
// Usage: one event, summed
// ---------------------------------------------------------------------------

// sumTranscribeUsage folds every chunk's billed usage into one payload.
//
// This is the decision the tool turns on. Result.GeminiUsage carries ONE
// payload, and the runner commits it as one usage event stamped with the
// sub-turn (internal/session/turn.go). Fifteen chunks could have become
// fifteen events instead — the store sums every usage event it finds
// (store.SessionUsageSummaries), so the session total would have been right
// either way. What would NOT have been right is what a human reads: the
// transcript's sub-turn card absorbs at most one usage block into its header
// and each later one for the same sub-turn REPLACES it (web/src/api/groups.ts,
// pushBlock's "usage" case). Fifteen events would put one chunk's cost on the
// card and drop the other fourteen from the display — a tool whose true cost
// is fifteen calls showing the price of one. Vision spend has already been a
// third of a run's cost once (docs/reviews/vision-path-2026-08-14.md), and
// under-reporting it by fifteen-sixteenths on the screen where it is noticed
// is a worse failure than losing the per-chunk breakdown, which nothing
// downstream reads and which the tool's own text carries anyway.
//
// Summing is exact rather than approximate: token counts add, and each
// chunk's cost is computed at its own instant by the price table before it
// gets here, so the total is the sum of what was actually billed and not a
// re-pricing of summed tokens. RateTier survives only when every chunk
// agrees, which for Gemini — one rate around the clock, no rate schedule in
// the price table — it always does; a future model with a peak rate that a
// fan-out straddled would blank it rather than claim one of the two.
func sumTranscribeUsage(results []transcribeResult) *store.UsagePayload {
	var total *store.UsagePayload
	for _, r := range results {
		if r.usage == nil {
			continue
		}
		if total == nil {
			copied := *r.usage
			total = &copied
			continue
		}
		total.PromptTokens += r.usage.PromptTokens
		total.PromptCacheHitTokens += r.usage.PromptCacheHitTokens
		total.PromptCacheMissTokens += r.usage.PromptCacheMissTokens
		total.CompletionTokens += r.usage.CompletionTokens
		total.ReasoningTokens += r.usage.ReasoningTokens
		total.CostUSD += r.usage.CostUSD
		total.Calls += r.usage.Calls
		if total.RateTier != r.usage.RateTier {
			total.RateTier = ""
		}
	}
	return total
}

// attachTranscribeUsage puts the summed payload on the result and appends the
// cost line, the way visionResult does for the single-call tools. It counts
// the calls in the line, because "this call cost $X" is what the other vision
// tools say and it would be wrong here in the direction that matters.
func attachTranscribeUsage(res *Result, usage *store.UsagePayload) {
	if usage == nil {
		return
	}
	res.GeminiUsage = usage
	if usage.CostUSD <= 0 {
		return
	}
	line := fmt.Sprintf("This call made %s and cost $%.4f in total on %s",
		plural(usage.Calls, "vision call"), usage.CostUSD, usage.Model)
	if usage.ReasoningTokens > 0 && usage.CompletionTokens > 0 {
		line += fmt.Sprintf(" (%d of %d output tokens were the model thinking)",
			usage.ReasoningTokens, usage.CompletionTokens)
	}
	res.Content += "\n\n" + line + "."
}

// ---------------------------------------------------------------------------
// Chunk planning: the ink profile and the cut bands
// ---------------------------------------------------------------------------

// transcribeChunk is one piece of the image, in the coordinates of whatever
// was handed to planChunks — the whole image, or the region cropped out of
// it. top/bottom are the pixels the chunk is cut from, overlap included;
// cutTop/cutBottom are the boundaries themselves, and margin is how many
// blank rows sat either side of the bottom cut. A margin of zero means the
// cut went through content and the seam needs a human's eye.
type transcribeChunk struct {
	index     int
	top       int
	bottom    int
	cutTop    int
	cutBottom int
	margin    int
	// overlap is the pixels this chunk shares with the NEXT one, on each
	// side of the boundary. Zero when the cut landed in clean space.
	overlap int
}

func (c transcribeChunk) height() int { return c.bottom - c.top }

// planChunks decides where to cut. It measures the ink in every row, then
// walks down the image choosing each cut inside a window around a target
// height, preferring the middle of the widest run of near-blank rows it can
// find there — so a line of text is never sliced through the middle.
func planChunks(img image.Image, maxChunks int) []transcribeChunk {
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width < 1 || height < 1 {
		return nil
	}
	target, minH, maxH := transcribeSplitSizes(width)
	if height <= maxH || maxChunks < 2 {
		// One chunk: the whole image fits inside the largest piece the
		// splitter would ever produce, so there is nothing to cut and
		// nothing to merge.
		return []transcribeChunk{{index: 0, top: 0, bottom: height, cutTop: 0, cutBottom: height}}
	}

	ink := inkProfile(img)
	blank := blankLevel(ink)

	var cuts []int
	var margins []int
	start := 0
	for {
		if height-start <= maxH {
			break
		}
		if len(cuts) >= maxChunks-1 {
			// The cap is on billed calls, so it is enforced by stopping the
			// split rather than by refusing the image: the last chunk keeps
			// whatever is left and is read at whatever resolution that
			// leaves it. The chunk heights in the report are where that
			// shows — one chunk far taller than its neighbours is the place
			// detail was traded for the cap.
			break
		}
		// Never leave a final sliver: if cutting at the target would leave
		// less than a minimum chunk behind, pull the window in so both
		// sides stay above the floor.
		lo := start + minH
		hi := start + maxH
		if limit := height - minH; hi > limit {
			hi = limit
		}
		if lo > hi {
			break
		}
		desired := start + target
		if desired < lo {
			desired = lo
		}
		if desired > hi {
			desired = hi
		}
		cut, margin := chooseCut(ink, lo, hi, desired, blank)
		cuts = append(cuts, cut)
		margins = append(margins, margin)
		start = cut
	}

	return chunksFromCuts(cuts, margins, height)
}

// chunksFromCuts turns boundary positions into chunks, giving each boundary
// the overlap its blank margin did not already provide.
func chunksFromCuts(cuts, margins []int, height int) []transcribeChunk {
	chunks := make([]transcribeChunk, 0, len(cuts)+1)
	prevCut := 0
	prevOverlap := 0
	for i := 0; i <= len(cuts); i++ {
		cutBottom := height
		overlap := 0
		margin := 0
		if i < len(cuts) {
			cutBottom = cuts[i]
			margin = margins[i]
			// The blank band around the cut IS the overlap, as far as
			// safety goes: if the boundary sits in blank rows then no glyph
			// spans it and there is nothing for the two chunks to see twice.
			// Only a cut that had to go through content buys real overlap.
			if margin < transcribeCleanMarginPx {
				overlap = transcribeOverlapPx
			}
		}
		top := prevCut - prevOverlap
		if top < 0 {
			top = 0
		}
		bottom := cutBottom + overlap
		if bottom > height {
			bottom = height
		}
		chunks = append(chunks, transcribeChunk{
			index: i, top: top, bottom: bottom,
			cutTop: prevCut, cutBottom: cutBottom,
			margin: margin, overlap: overlap,
		})
		prevCut = cutBottom
		prevOverlap = overlap
	}
	return chunks
}

// transcribeSplitSizes is upstream's resolve_split_sizes for the general
// (non-chat) mode, with its constants. The target scales with width because
// a wide page has wide lines: the aspect ratio the vision model reads best
// is roughly a page, not a ribbon.
func transcribeSplitSizes(width int) (target, minH, maxH int) {
	target = clampInt(int(float64(width)*1.45), 1200, 2400)
	minH = maxInt(600, int(float64(target)*0.58))
	maxH = minInt(3400, int(float64(target)*1.42))
	return target, minH, maxH
}

// inkProfile counts, for each row, how many sampled pixels differ from that
// row's own background.
//
// The background is taken from the row's left and right margins rather than
// from a global colour, which is what makes this work on a dark theme, on a
// page with a vertical gradient, and on a capture with a coloured hero band:
// each row is judged against what its own edges look like. A row inside a
// full-width block of solid colour reads as blank, correctly — there is no
// text on it to cut through.
func inkProfile(img image.Image) []int {
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	edge := clampInt(width/18, 1, 24)
	stride := maxInt(1, width/transcribeAnalysisColumns)

	ink := make([]int, height)
	for y := 0; y < height; y++ {
		py := bounds.Min.Y + y
		var br, bg, bb, n int
		for x := 0; x < edge; x++ {
			r, g, b := rgb8(img, bounds.Min.X+x, py)
			br, bg, bb, n = br+r, bg+g, bb+b, n+1
			r, g, b = rgb8(img, bounds.Max.X-1-x, py)
			br, bg, bb, n = br+r, bg+g, bb+b, n+1
		}
		if n == 0 {
			continue
		}
		br, bg, bb = br/n, bg/n, bb/n
		count := 0
		for x := 0; x < width; x += stride {
			r, g, b := rgb8(img, bounds.Min.X+x, py)
			if maxInt(absInt(r-br), maxInt(absInt(g-bg), absInt(b-bb))) >= transcribeInkThreshold {
				count++
			}
		}
		ink[y] = count
	}
	return ink
}

// rgb8 reads one pixel as three 8-bit channels. image.Image.At returns
// 16-bit alpha-premultiplied values through an interface; this is the one
// conversion the whole profile needs, kept in one place.
func rgb8(img image.Image, x, y int) (int, int, int) {
	r, g, b, _ := img.At(x, y).RGBA()
	return int(r >> 8), int(g >> 8), int(b >> 8)
}

// blankLevel is the ink count at or below which a row counts as empty. It is
// a fraction of the sampled columns rather than zero: antialiasing leaves a
// pixel or two of ink on a row that is blank to the eye, and a threshold of
// zero finds no blank rows on any real screenshot.
func blankLevel(ink []int) int {
	peak := 0
	for _, v := range ink {
		if v > peak {
			peak = v
		}
	}
	return maxInt(1, peak/100)
}

// chooseCut picks the row to cut at within [lo, hi].
//
// It prefers the centre of the widest run of blank rows in the window,
// discounted by how far that run sits from the desired height — a slightly
// off-target cut through clean space beats an on-target cut through a
// paragraph. With no blank run at all (a dense table, a screenshot of a
// terminal), it falls back to the quietest single row nearest the target and
// reports a margin of zero, which is what marks the seam for review.
func chooseCut(ink []int, lo, hi, desired, blank int) (cut, margin int) {
	lo = clampInt(lo, 0, len(ink)-1)
	hi = clampInt(hi, 0, len(ink)-1)
	if lo >= hi {
		return hi, 0
	}

	bestScore := 0.0
	bestCut, bestMargin := -1, 0
	span := float64(maxInt(1, hi-lo))
	for y := lo; y <= hi; {
		if ink[y] > blank {
			y++
			continue
		}
		runStart := y
		for y <= hi && ink[y] <= blank {
			y++
		}
		runEnd := y - 1
		centre := (runStart + runEnd) / 2
		half := minInt(centre-runStart, runEnd-centre)
		// Width past transcribeWidestBandPx buys nothing: the cut is already
		// clean, and a wider band does not make it cleaner.
		width := float64(minInt(half, transcribeWidestBandPx)) / float64(transcribeWidestBandPx)
		distance := float64(absInt(centre-desired)) / span
		score := width - distance*0.5
		if bestCut < 0 || score > bestScore {
			bestScore, bestCut, bestMargin = score, centre, half
		}
	}
	if bestCut >= 0 {
		return bestCut, bestMargin
	}

	quietest := lo
	for y := lo; y <= hi; y++ {
		if ink[y] < ink[quietest] ||
			(ink[y] == ink[quietest] && absInt(y-desired) < absInt(quietest-desired)) {
			quietest = y
		}
	}
	return quietest, 0
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// ---------------------------------------------------------------------------
// Fan-out
// ---------------------------------------------------------------------------

type transcribeResult struct {
	chunk transcribeChunk
	lines []string
	usage *store.UsagePayload
	err   error
}

// encodeChunks cuts each planned chunk out of the source and encodes it,
// downscaling any that lands over the per-file byte cap the way the other
// vision tools do. Every chunk goes at high resolution: buying resolution is
// the entire point of cutting the image up, and sending the later ones at
// medium would hand back most of what the split just bought.
func encodeChunks(ctx context.Context, e *Executor, src image.Image, plan []transcribeChunk, mimeType, userPath string) ([]gemini.Image, []string, error) {
	bounds := src.Bounds()
	images := make([]gemini.Image, 0, len(plan))
	var notes []string
	maxBytes := int64(e.visionMaxBytes(ctx))
	base := filepath.Base(userPath)
	for _, c := range plan {
		piece := cropAndScale(src, [4]int{0, c.top, bounds.Dx(), c.bottom}, 1)
		encoded, encodedMIME, err := encodeScreenshot(piece, mimeType)
		if err != nil {
			return nil, nil, fmt.Errorf("encode chunk %d of %s: %v", c.index+1, userPath, err)
		}
		if int64(len(encoded)) > maxBytes {
			shrunk, shrunkMIME, w, h, err := downscaleImage(encoded, encodedMIME, maxBytes)
			if err != nil {
				return nil, nil, fmt.Errorf("chunk %d of %s is %d bytes, over the %d-byte per-file limit, and could not be downscaled: %v",
					c.index+1, userPath, len(encoded), maxBytes, err)
			}
			notes = append(notes, fmt.Sprintf("chunk %d was downscaled to %dx%d to fit the %d-byte per-file limit",
				c.index+1, w, h, maxBytes))
			encoded, encodedMIME = shrunk, shrunkMIME
		}
		images = append(images, gemini.Image{
			Data:       encoded,
			MIMEType:   encodedMIME,
			Resolution: gemini.ResolutionHigh,
			Label:      fmt.Sprintf("%s chunk %d of %d", base, c.index+1, len(plan)),
		})
	}
	return images, notes, nil
}

// runTranscribeChunks sends every chunk concurrently, bounded by the
// configured concurrency, and returns one result per chunk in plan order.
//
// The context is the tool's own, so a cancelled or timed-out call stops every
// outstanding request rather than leaving them to finish into a result
// nobody will read. Chunks that already came back keep their usage: they were
// billed.
func (e *Executor) runTranscribeChunks(ctx context.Context, plan []transcribeChunk, images []gemini.Image) []transcribeResult {
	results := make([]transcribeResult, len(plan))
	limit := clampInt(e.transcribeConcurrency(ctx), 1, len(plan))
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup

	for i := range plan {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			results[i].chunk = plan[i]
			if ctx.Err() != nil {
				results[i].err = ctx.Err()
				return
			}
			// Transcription is perception, not reasoning: the model is
			// copying what it can see, and thinking budget spent on a
			// transcript is budget spent deciding what the text means.
			answer, usage, sentAt, err := visionInteract(ctx, e, "",
				transcribePrompt(i+1, len(plan)), []gemini.Image{images[i]},
				e.visionThinkingLevel(ctx, gemini.ThinkingLevelLow))
			if err != nil {
				results[i].err = err
				return
			}
			results[i].lines = trimOuterBlankLines(answer)
			if usage != nil {
				results[i].usage = e.transcribeUsagePayload(sentAt, usage)
			}
		}(i)
	}
	wg.Wait()
	return results
}

// transcribeUsagePayload prices one chunk's call, the same way visionResult
// prices a Glance. Calls is set to one here and summed by sumTranscribeUsage.
func (e *Executor) transcribeUsagePayload(sentAt time.Time, usage *gemini.Usage) *store.UsagePayload {
	model := gemini.DefaultModel
	if e.GeminiModel != nil {
		if m, err := e.GeminiModel(); err == nil && m != "" {
			model = m
		}
	}
	payload := geminiUsagePayload(e.Prices, model, sentAt, usage)
	payload.Calls = 1
	return payload
}

// transcribePrompt is Glance's OCR instruction with upstream's chunk note
// and its ordering requirements added. Two things it must say and does:
// that this is one section of a taller image, so the model does not try to
// summarise a document it can only see part of; and that it must not infer
// what is clipped, because an inferred line at a boundary is exactly the
// fabrication the seam audit cannot distinguish from a real one.
func transcribePrompt(index, total int) string {
	return "Transcribe every piece of visible text in this image verbatim (titles, body text, labels, " +
		"watermarks, etc.), line by line, without omitting any characters. Do not rewrite, summarize, or " +
		"translate the text, and do not add any preamble, explanation, or extra content. " +
		"Keep the visible top-to-bottom reading order and preserve wording, punctuation, line breaks, " +
		"labels, timestamps, headings, lists, tables, code, quoted text, and paragraph order. " +
		"Do not infer clipped or hidden content; write [unreadable] only where visible text cannot be read. " +
		fmt.Sprintf("This is section %d of %d cut from one tall, vertically scrolling image.", index, total)
}

// ---------------------------------------------------------------------------
// The merge and the seam audit
// ---------------------------------------------------------------------------

// seam is one boundary between two chunks, and what happened there.
type seam struct {
	after   int // 1-based chunk index above the boundary
	cutY    int // in the source image's coordinates, offset applied by the caller
	margin  int // blank rows either side of the cut
	overlap int // pixels of the neighbour each chunk saw, per side
	removed int // lines the merge dropped as a repeat
	method  string
}

// needsReview is the audit's whole judgement, and it is deliberately blunt:
// a boundary is worth a human's eye exactly when overlap was sent, which is
// exactly when no usable blank band could be found and the cut went through
// content. Upstream flags every overlap-bearing boundary the same way, and
// the reason is that this tool cannot check its own merge. A cut through a
// line of text leaves each chunk transcribing a different half of it; if
// those halves happen to match, the merge deletes one and the result is
// wrong and silent, and if they do not match, the result is wrong and
// silent in the other direction. Whether the exact match fired says
// something — and the report prints it — but it is not evidence enough to
// call the seam sound.
//
// The good case, and the ordinary one on a page of prose, is a cut in the
// leading between two lines: no overlap, nothing removed, nothing to check,
// because no glyph crossed the boundary in the first place.
func (s seam) needsReview() bool {
	return s.overlap > 0
}

// describe says what happened at the boundary in the terms that decide
// whether it is sound: where the cut landed, whether the two chunks were
// shown the same pixels, and what the merge did about it.
func (s seam) describe() string {
	if s.overlap == 0 {
		return fmt.Sprintf("cut inside a %dpx blank band, no overlap needed, nothing to remove", s.margin)
	}
	if s.removed == 0 {
		return fmt.Sprintf("no blank band to cut in, %dpx of overlap sent, but the two chunks did not "+
			"transcribe it the same way so nothing was removed", s.overlap)
	}
	return fmt.Sprintf("no blank band to cut in, %dpx of overlap sent, %s removed as an exact repeat",
		s.overlap, plural(s.removed, "line"))
}

// mergeTranscripts joins the chunks in order, dropping from each one the run
// of opening lines that repeats the tail of what is already merged.
//
// The match is exact on normalised lines and nothing else. A ratio-based
// fuzzy match (upstream falls back to difflib.SequenceMatcher) would delete
// more at a seam, and every extra deletion it makes is a line the caller
// never sees and cannot know was there. Under-deleting leaves a visible
// duplicate; over-deleting leaves silence.
func mergeTranscripts(results []transcribeResult) (string, []seam) {
	var merged []string
	var seams []seam
	for i, r := range results {
		lines := r.lines
		if i == 0 {
			merged = append(merged, lines...)
			continue
		}
		prev := results[i-1].chunk
		s := seam{after: i, cutY: prev.cutBottom, margin: prev.margin, overlap: prev.overlap}
		if prev.overlap > 0 {
			s.removed = findLineOverlap(merged, lines)
			if s.removed > 0 {
				s.method = "exact"
			} else {
				s.method = "none"
			}
		} else {
			// No pixels were shown twice, so no line can legitimately be a
			// repeat, and a dedupe here could only delete a real one — a
			// table with two identical rows either side of the boundary is
			// the obvious way to lose data to an over-eager merge.
			s.method = "not-needed"
		}
		merged = append(merged, lines[s.removed:]...)
		seams = append(seams, s)
	}
	return strings.Join(merged, "\n"), seams
}

// findLineOverlap returns how many of current's opening lines repeat the tail
// of previous, longest run first.
func findLineOverlap(previous, current []string) int {
	most := minInt(transcribeMaxOverlapLines, minInt(len(previous), len(current)))
	for n := most; n >= 1; n-- {
		left := previous[len(previous)-n:]
		right := current[:n]
		matched, meaningful := true, false
		for i := range left {
			l, r := normalizedLine(left[i]), normalizedLine(right[i])
			if l != r {
				matched = false
				break
			}
			if l != "" {
				meaningful = true
			}
		}
		// A run of blank lines matches itself and means nothing; requiring
		// one line with content in it stops the merge deleting a paragraph
		// break and calling it a repeat.
		if matched && meaningful {
			return n
		}
	}
	return 0
}

// normalizedLine is what two lines are compared as: case-folded, with runs of
// whitespace collapsed. Nothing else — no punctuation stripping, no unicode
// folding beyond case. Every normalisation makes two different lines more
// likely to compare equal, and equal means one of them gets deleted.
func normalizedLine(line string) string {
	return strings.ToLower(strings.Join(strings.Fields(line), " "))
}

// trimOuterBlankLines splits a chunk's answer into lines with trailing
// whitespace and outer blank lines removed, so a model that opened or closed
// with a blank line does not shift the seam match.
func trimOuterBlankLines(text string) []string {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// ---------------------------------------------------------------------------
// The report
// ---------------------------------------------------------------------------

// transcribeReport is the header every call returns ahead of the transcript:
// what was cut where, what the merge did at each boundary, and which
// boundaries to check. It names y positions in the ORIGINAL image's
// coordinates, so a boundary marked for review can be cut out with Crop and
// looked at without any arithmetic.
func transcribeReport(args transcribeArgs, width, height, offset int, plan []transcribeChunk, seams []seam, notes []string) string {
	var b strings.Builder
	scope := fmt.Sprintf("%dx%d", width, height)
	if args.Region != "" {
		scope += fmt.Sprintf(" region %s of", args.Region)
	}
	if len(plan) == 1 {
		fmt.Fprintf(&b, "Transcribed %s (%s) at full resolution. It was short enough to read in one "+
			"call, so it was not cut up and there are no seams.", args.ImagePath, scope)
	} else {
		fmt.Fprintf(&b, "Transcribed %s (%s) in %s, each read at full resolution.",
			args.ImagePath, scope, plural(len(plan), "chunk"))
	}
	for _, note := range notes {
		b.WriteString("\n" + note)
	}

	if len(plan) > 1 {
		b.WriteString("\n\nChunks, in the source image's own y coordinates:")
		for _, c := range plan {
			fmt.Fprintf(&b, "\n  %d. y %d-%d (%dpx)", c.index+1, c.top+offset, c.bottom+offset, c.height())
		}
		b.WriteString("\n\nSeams — where two chunks were joined, and what the merge removed there:")
		review := 0
		for _, s := range seams {
			flag := "ok"
			if s.needsReview() {
				flag = "CHECK"
				review++
			}
			fmt.Fprintf(&b, "\n  %d→%d at y=%d: %s — %s", s.after, s.after+1, s.cutY+offset, s.describe(), flag)
		}
		if review == 0 {
			b.WriteString("\n\nEvery cut landed in blank space, so nothing at a boundary could be lost or duplicated.")
		} else {
			fmt.Fprintf(&b, "\n\n%s marked CHECK: no blank band was available there, so the cut went through content and "+
				"each chunk may have transcribed a different half of the same line. Both chunks were shown the strip "+
				"either side of the cut; what the merge could match there is in the line above. Before relying on the "+
				"text at those positions, Crop the source around the y and Glance the result. A merge that goes wrong "+
				"drops or repeats a line silently — it does not produce anything that looks broken.",
				plural(review, "seam"))
		}
	}
	return b.String()
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
