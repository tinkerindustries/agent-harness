package tools

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/gemini"
	"github.com/mrgeoffrich/agent-harness/internal/gemini/geminitest"
	"github.com/mrgeoffrich/agent-harness/internal/store"
)

// ---------------------------------------------------------------------------
// A synthetic page
// ---------------------------------------------------------------------------

// pageLayout describes a fake document: bands of "text" separated by blank
// gutters, on a white background with white margins down both sides. It is
// the shape the cut search is meant to exploit, and knowing where the
// gutters are is what lets a test assert that a cut landed in one.
type pageLayout struct {
	width   int
	bandH   int
	gutterH int
	bands   int
}

func (p pageLayout) height() int { return p.bands*p.bandH + (p.bands-1)*p.gutterH }

// gutterRows reports whether y falls inside a gutter — the rows a cut is
// allowed to land on.
func (p pageLayout) gutterRows(y int) bool {
	period := p.bandH + p.gutterH
	if y >= p.bands*period-p.gutterH {
		return false
	}
	return y%period >= p.bandH
}

// render draws the layout: black glyph-ish blocks across the middle of each
// band, white everywhere else.
func (p pageLayout) render() image.Image {
	img := image.NewRGBA(image.Rect(0, 0, p.width, p.height()))
	white := color.RGBA{255, 255, 255, 255}
	black := color.RGBA{0, 0, 0, 255}
	for y := 0; y < p.height(); y++ {
		for x := 0; x < p.width; x++ {
			img.Set(x, y, white)
		}
	}
	period := p.bandH + p.gutterH
	for y := 0; y < p.height(); y++ {
		if y%period >= p.bandH {
			continue // gutter
		}
		// A margin either side, so the row's background reference (its left
		// and right edges) is the page colour and not the text.
		for x := 100; x < p.width-100; x += 3 {
			img.Set(x, y, black)
			img.Set(x+1, y, black)
		}
	}
	return img
}

func (p pageLayout) writePNG(t *testing.T, path string) {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, p.render()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// Cut placement
// ---------------------------------------------------------------------------

// TestPlanChunksCutsInGutters is the property the whole tool rests on: a cut
// never goes through a line of text. Everything downstream — the merge, the
// seam audit, the caller's trust in the transcript — assumes it, and a cut
// through a glyph produces two half-lines that no merge can reassemble.
func TestPlanChunksCutsInGutters(t *testing.T) {
	page := pageLayout{width: 800, bandH: 30, gutterH: 20, bands: 90}
	img := page.render()
	plan := planChunks(img, transcribeMaxChunksDefault)

	if len(plan) < 3 {
		t.Fatalf("a %dpx page should split into several chunks, got %d", page.height(), len(plan))
	}
	for _, c := range plan[:len(plan)-1] {
		if !page.gutterRows(c.cutBottom) {
			t.Errorf("chunk %d cut at y=%d, which is inside a band of text", c.index+1, c.cutBottom)
		}
		if c.margin <= 0 {
			t.Errorf("chunk %d cut at y=%d reported margin %d; a gutter cut should have blank rows either side",
				c.index+1, c.cutBottom, c.margin)
		}
	}
}

// TestPlanChunksCoverTheImageExactly pins the arithmetic that a lost or
// double-counted row would hide in: the cuts partition the image, and each
// chunk's overlap only ever extends what it reads, never what it owns.
func TestPlanChunksCoverTheImageExactly(t *testing.T) {
	page := pageLayout{width: 800, bandH: 30, gutterH: 20, bands: 90}
	plan := planChunks(page.render(), transcribeMaxChunksDefault)
	height := page.height()

	if plan[0].cutTop != 0 {
		t.Errorf("first chunk starts at %d, want 0", plan[0].cutTop)
	}
	if last := plan[len(plan)-1]; last.cutBottom != height {
		t.Errorf("last chunk ends at %d, want %d", last.cutBottom, height)
	}
	for i, c := range plan {
		if c.top > c.cutTop || c.bottom < c.cutBottom {
			t.Errorf("chunk %d reads y %d-%d but owns y %d-%d; a chunk must read at least what it owns",
				i+1, c.top, c.bottom, c.cutTop, c.cutBottom)
		}
		if c.top < 0 || c.bottom > height {
			t.Errorf("chunk %d reads y %d-%d, outside the image", i+1, c.top, c.bottom)
		}
		if i > 0 && plan[i-1].cutBottom != c.cutTop {
			t.Errorf("gap or overlap between chunk %d (ends %d) and %d (starts %d)",
				i, plan[i-1].cutBottom, i+1, c.cutTop)
		}
	}
}

// TestPlanChunksSingleChunkForAShortImage: a viewport-sized capture is not a
// long screenshot, and must not pay for a split it does not need.
func TestPlanChunksSingleChunkForAShortImage(t *testing.T) {
	page := pageLayout{width: 800, bandH: 30, gutterH: 20, bands: 10} // 470px
	plan := planChunks(page.render(), transcribeMaxChunksDefault)
	if len(plan) != 1 {
		t.Fatalf("a %dpx image split into %d chunks, want 1", page.height(), len(plan))
	}
	if plan[0].overlap != 0 || plan[0].cutBottom != page.height() {
		t.Errorf("single chunk should own the whole image with no overlap, got %+v", plan[0])
	}
}

// TestPlanChunksHonoursTheChunkCap: the cap bounds billed calls, so it must
// hold even on an image tall enough to want more.
func TestPlanChunksHonoursTheChunkCap(t *testing.T) {
	page := pageLayout{width: 800, bandH: 30, gutterH: 20, bands: 300} // ~14,950px
	for _, cap := range []int{1, 2, 5} {
		plan := planChunks(page.render(), cap)
		if len(plan) > cap {
			t.Errorf("cap %d produced %d chunks", cap, len(plan))
		}
		if last := plan[len(plan)-1]; last.cutBottom != page.height() {
			t.Errorf("cap %d: last chunk ends at %d, want the whole image (%d)",
				cap, last.cutBottom, page.height())
		}
	}
}

// TestChooseCutWithNoBlankRowsReportsZeroMargin: a dense image (a terminal
// capture, a solid table) has no gutter to cut in. The tool must still cut —
// and must say the cut was not clean, because that is the signal the seam
// audit turns into CHECK.
func TestChooseCutWithNoBlankRowsReportsZeroMargin(t *testing.T) {
	ink := make([]int, 400)
	for i := range ink {
		ink[i] = 50 + i%7 // never quiet, mildly varied
	}
	cut, margin := chooseCut(ink, 100, 300, 200, blankLevel(ink))
	if cut < 100 || cut > 300 {
		t.Errorf("cut %d outside the window", cut)
	}
	if margin != 0 {
		t.Errorf("margin = %d, want 0 when no blank band exists", margin)
	}
}

// TestInkProfileReadsAgainstTheRowsOwnBackground: the profile has to work on
// a dark theme, which it does by comparing each row to its own left and right
// margins rather than to a fixed colour. Inverted, the same page must produce
// the same blank rows.
func TestInkProfileReadsAgainstTheRowsOwnBackground(t *testing.T) {
	page := pageLayout{width: 800, bandH: 30, gutterH: 20, bands: 6}
	light := page.render()
	dark := image.NewRGBA(light.Bounds())
	for y := light.Bounds().Min.Y; y < light.Bounds().Max.Y; y++ {
		for x := light.Bounds().Min.X; x < light.Bounds().Max.X; x++ {
			r, g, b, _ := light.At(x, y).RGBA()
			dark.Set(x, y, color.RGBA{255 - uint8(r>>8), 255 - uint8(g>>8), 255 - uint8(b>>8), 255})
		}
	}
	lightInk, darkInk := inkProfile(light), inkProfile(dark)
	for y := range lightInk {
		if (lightInk[y] == 0) != (darkInk[y] == 0) {
			t.Fatalf("row %d: light ink %d, dark ink %d — the profile must not depend on the theme",
				y, lightInk[y], darkInk[y])
		}
	}
	if !page.gutterRows(35) || lightInk[35] != 0 {
		t.Errorf("row 35 is a gutter but reads %d ink", lightInk[35])
	}
	if page.gutterRows(10) || lightInk[10] == 0 {
		t.Errorf("row 10 is text but reads no ink")
	}
}

// ---------------------------------------------------------------------------
// The merge
// ---------------------------------------------------------------------------

func chunkWith(index, overlap int, lines ...string) transcribeResult {
	return transcribeResult{
		chunk: transcribeChunk{index: index, overlap: overlap, cutBottom: 100 * (index + 1), margin: 9},
		lines: lines,
	}
}

func TestMergeTranscripts(t *testing.T) {
	t.Run("removes the repeated tail when the chunks overlapped", func(t *testing.T) {
		merged, seams := mergeTranscripts([]transcribeResult{
			chunkWith(0, 40, "alpha", "bravo", "charlie"),
			chunkWith(1, 0, "bravo", "charlie", "delta"),
		})
		if merged != "alpha\nbravo\ncharlie\ndelta" {
			t.Errorf("merged = %q", merged)
		}
		if len(seams) != 1 || seams[0].removed != 2 || seams[0].method != "exact" {
			t.Errorf("seams = %+v, want one exact seam removing 2 lines", seams)
		}
		if !seams[0].needsReview() {
			t.Error("overlap was sent, so the cut went through content; the seam is worth a look even though the match fired")
		}
	})

	t.Run("removes nothing when no pixels were shown twice", func(t *testing.T) {
		// The two chunks genuinely both contain "total" — a table with the
		// same row either side of a clean cut. With no overlap sent, there is
		// no repeat to remove and deleting one would lose real data.
		merged, seams := mergeTranscripts([]transcribeResult{
			chunkWith(0, 0, "alpha", "total"),
			chunkWith(1, 0, "total", "delta"),
		})
		if merged != "alpha\ntotal\ntotal\ndelta" {
			t.Errorf("merged = %q, want both totals kept", merged)
		}
		if seams[0].method != "not-needed" || seams[0].removed != 0 {
			t.Errorf("seam = %+v, want not-needed removing nothing", seams[0])
		}
		if seams[0].needsReview() {
			t.Error("a clean cut with no overlap is the good case and should not be flagged")
		}
	})

	t.Run("matches case and whitespace insensitively", func(t *testing.T) {
		merged, seams := mergeTranscripts([]transcribeResult{
			chunkWith(0, 40, "alpha", "The  Quick Brown Fox"),
			chunkWith(1, 0, "the quick brown fox", "delta"),
		})
		if merged != "alpha\nThe  Quick Brown Fox\ndelta" {
			t.Errorf("merged = %q", merged)
		}
		if seams[0].removed != 1 {
			t.Errorf("removed = %d, want 1", seams[0].removed)
		}
	})

	t.Run("a run of blank lines is not a repeat", func(t *testing.T) {
		// Both chunks end and begin with a blank line. Matching on those
		// alone would silently swallow a paragraph break every time.
		merged, seams := mergeTranscripts([]transcribeResult{
			{chunk: transcribeChunk{overlap: 40}, lines: []string{"alpha", ""}},
			{chunk: transcribeChunk{}, lines: []string{"", "bravo"}},
		})
		if merged != "alpha\n\n\nbravo" {
			t.Errorf("merged = %q, want nothing removed", merged)
		}
		if seams[0].removed != 0 || seams[0].method != "none" {
			t.Errorf("seam = %+v, want no match", seams[0])
		}
	})

	t.Run("an overlap that matched nothing is flagged for review", func(t *testing.T) {
		_, seams := mergeTranscripts([]transcribeResult{
			chunkWith(0, 40, "alpha", "bravo"),
			chunkWith(1, 0, "charlie", "delta"),
		})
		if !seams[0].needsReview() {
			t.Error("pixels were shown to both chunks and neither transcribed them the same way; that must be flagged")
		}
	})

	t.Run("prefers the longest matching run", func(t *testing.T) {
		merged, seams := mergeTranscripts([]transcribeResult{
			chunkWith(0, 40, "x", "a", "b", "a", "b"),
			chunkWith(1, 0, "a", "b", "a", "b", "y"),
		})
		if merged != "x\na\nb\na\nb\ny" {
			t.Errorf("merged = %q", merged)
		}
		if seams[0].removed != 4 {
			t.Errorf("removed = %d, want the longest run of 4", seams[0].removed)
		}
	})
}

// TestTranscribeReportReadsAsEnglishForOneChunk: a live run's first result
// said "in 1 chunk, each read at full resolution" and "These 1 vision calls
// cost", which is the kind of wrong that makes a reader distrust the numbers
// beside it.
func TestTranscribeReportReadsAsEnglishForOneChunk(t *testing.T) {
	report := transcribeReport(transcribeArgs{ImagePath: "page.png"}, 800, 600, 0,
		[]transcribeChunk{{index: 0, cutBottom: 600, bottom: 600}}, nil, nil)
	for _, bad := range []string{"each read", "1 chunks", "Seams"} {
		if strings.Contains(report, bad) {
			t.Errorf("a one-chunk report should not contain %q:\n%s", bad, report)
		}
	}
	if !strings.Contains(report, "there are no seams") {
		t.Errorf("a one-chunk report should say why there is no seam list:\n%s", report)
	}

	var res Result
	attachTranscribeUsage(&res, &store.UsagePayload{Calls: 1, CostUSD: 0.0016, Model: "gemini-3.7-flash"})
	if !strings.Contains(res.Content, "1 vision call ") {
		t.Errorf("singular cost line reads wrong: %q", res.Content)
	}
	res = Result{}
	attachTranscribeUsage(&res, &store.UsagePayload{Calls: 3, CostUSD: 0.005, Model: "gemini-3.7-flash"})
	if !strings.Contains(res.Content, "3 vision calls ") {
		t.Errorf("plural cost line reads wrong: %q", res.Content)
	}
}

func TestSeamNeedsReview(t *testing.T) {
	cases := []struct {
		name string
		s    seam
		want bool
	}{
		{"clean cut, no overlap needed", seam{margin: 20, overlap: 0, removed: 0}, false},
		// A forced cut is flagged whether or not the exact match fired: the
		// tool cannot tell a correct merge from a plausible one, and a
		// wrongly merged seam reads as ordinary prose.
		{"overlap sent and matched", seam{margin: 0, overlap: 48, removed: 2}, true},
		{"overlap sent and unmatched", seam{margin: 0, overlap: 48, removed: 0}, true},
	}
	for _, tc := range cases {
		if got := tc.s.needsReview(); got != tc.want {
			t.Errorf("%s: needsReview() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestTrimOuterBlankLines(t *testing.T) {
	got := trimOuterBlankLines("\n\n  alpha  \r\nbravo\t\n\n\n")
	want := []string{"  alpha", "bravo"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// Usage
// ---------------------------------------------------------------------------

// TestSumTranscribeUsage is the accounting decision under test: many calls,
// one event. If this stops summing, the session's cost total under-reports
// the vision path by however many chunks it dropped.
func TestSumTranscribeUsage(t *testing.T) {
	results := []transcribeResult{
		{usage: &store.UsagePayload{Model: "gemini-3.7-flash", PromptTokens: 1120, PromptCacheMissTokens: 1120,
			CompletionTokens: 300, ReasoningTokens: 120, CostUSD: 0.0021, RateTier: "flat", Calls: 1}},
		{usage: &store.UsagePayload{Model: "gemini-3.7-flash", PromptTokens: 1150, PromptCacheMissTokens: 1150,
			CompletionTokens: 260, ReasoningTokens: 100, CostUSD: 0.0019, RateTier: "flat", Calls: 1}},
		{err: fmt.Errorf("never got a reply")}, // contributes nothing
	}
	got := sumTranscribeUsage(results)
	if got == nil {
		t.Fatal("nil usage from two billed calls")
	}
	if got.Calls != 2 {
		t.Errorf("Calls = %d, want 2", got.Calls)
	}
	if got.PromptTokens != 2270 || got.PromptCacheMissTokens != 2270 {
		t.Errorf("prompt tokens = %d/%d, want 2270/2270", got.PromptTokens, got.PromptCacheMissTokens)
	}
	if got.CompletionTokens != 560 || got.ReasoningTokens != 220 {
		t.Errorf("output tokens = %d completion / %d reasoning, want 560/220", got.CompletionTokens, got.ReasoningTokens)
	}
	if fmt.Sprintf("%.4f", got.CostUSD) != "0.0040" {
		t.Errorf("cost = %v, want 0.0040", got.CostUSD)
	}
	if got.RateTier != "flat" {
		t.Errorf("RateTier = %q, want flat", got.RateTier)
	}
	if got.Model != "gemini-3.7-flash" {
		t.Errorf("Model = %q", got.Model)
	}
}

// TestSumTranscribeUsageBlanksADisagreeingRateTier: summing two costs billed
// at different rates leaves a total that was billed at neither, so the field
// says nothing rather than naming one of them.
func TestSumTranscribeUsageBlanksADisagreeingRateTier(t *testing.T) {
	got := sumTranscribeUsage([]transcribeResult{
		{usage: &store.UsagePayload{RateTier: "peak", CostUSD: 1, Calls: 1}},
		{usage: &store.UsagePayload{RateTier: "off_peak", CostUSD: 1, Calls: 1}},
	})
	if got.RateTier != "" {
		t.Errorf("RateTier = %q, want empty when the chunks disagree", got.RateTier)
	}
}

func TestSumTranscribeUsageIsNilWhenNothingWasBilled(t *testing.T) {
	if got := sumTranscribeUsage([]transcribeResult{{err: fmt.Errorf("boom")}}); got != nil {
		t.Errorf("got %+v, want nil", got)
	}
}

// ---------------------------------------------------------------------------
// End to end, against a stubbed Gemini
// ---------------------------------------------------------------------------

var chunkLabelRE = regexp.MustCompile(`chunk (\d+) of (\d+)`)

// transcribeServer stubs Gemini for a chunked call. answer is asked what to
// reply for chunk n of total; the label the tool attaches to each image is
// what identifies it, so this also proves the chunks go out labelled.
func transcribeServer(t *testing.T, usageJSON string, answer func(n, total int) (string, error)) (*httptest.Server, func() int) {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		m := chunkLabelRE.FindStringSubmatch(string(body))
		if m == nil {
			t.Errorf("request carried no chunk label")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var n, total int
		fmt.Sscanf(m[1], "%d", &n)
		fmt.Sscanf(m[2], "%d", &total)

		mu.Lock()
		calls++
		mu.Unlock()

		text, err := answer(n, total)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"error":{"message":"stub failure"}}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, geminitest.Answer(text, usageJSON))
	}))
	t.Cleanup(srv.Close)
	return srv, func() int {
		mu.Lock()
		defer mu.Unlock()
		return calls
	}
}

const transcribeUsageJSON = `{"total_input_tokens":1120,"total_output_tokens":200,"total_thought_tokens":80,"total_tokens":1400}`

// TestTranscribeEndToEnd is the whole contract in one test: a tall image goes
// out as several labelled chunks, comes back as one document in reading
// order with the seam repeats removed, and bills one summed usage event.
func TestTranscribeEndToEnd(t *testing.T) {
	page := pageLayout{width: 800, bandH: 30, gutterH: 20, bands: 90}
	e, root := newTestExecutor(t)
	page.writePNG(t, filepath.Join(root, "page.png"))

	srv, callCount := transcribeServer(t, transcribeUsageJSON, func(n, total int) (string, error) {
		// Every cut on this page lands in a gutter, so no overlap is sent
		// and no chunk sees any of its neighbour's text: each transcribes
		// its own three lines and nothing else.
		var b strings.Builder
		for i := 0; i < 3; i++ {
			fmt.Fprintf(&b, "line %d\n", n*3+i)
		}
		return b.String(), nil
	})
	e.Gemini = reviewScreenshotClient(srv)

	res := runTool(t, e, "Transcribe", transcribeArgs{ImagePath: "page.png"})
	if res.IsError {
		t.Fatalf("Transcribe failed: %s", res.Content)
	}
	chunks := callCount()
	if chunks < 3 {
		t.Fatalf("a %dpx page produced only %d calls", page.height(), chunks)
	}

	// One usage event, covering every call.
	if res.GeminiUsage == nil {
		t.Fatal("no usage on the result")
	}
	if res.GeminiUsage.Calls != chunks {
		t.Errorf("usage Calls = %d, want %d", res.GeminiUsage.Calls, chunks)
	}
	if want := 1120 * chunks; res.GeminiUsage.PromptTokens != want {
		t.Errorf("prompt tokens = %d, want %d (%d chunks x 1120)",
			res.GeminiUsage.PromptTokens, want, chunks)
	}

	// The transcript reads in order with no line lost and none doubled.
	for i := 3; i < 3*chunks+3; i++ {
		line := fmt.Sprintf("line %d\n", i)
		if got := strings.Count(res.Content+"\n", line); got != 1 {
			t.Errorf("%q appears %d times, want exactly 1", strings.TrimSpace(line), got)
		}
	}
	if idx3, idx6 := strings.Index(res.Content, "line 3"), strings.Index(res.Content, "line 6"); idx3 > idx6 {
		t.Error("chunks were assembled out of order")
	}

	// And the report is there, ahead of the text, naming every seam.
	if !strings.Contains(res.Content, "Seams") {
		t.Error("the result carries no seam report")
	}
	for i := 1; i < chunks; i++ {
		if !strings.Contains(res.Content, fmt.Sprintf("%d\u2192%d at y=", i, i+1)) {
			t.Errorf("seam %d|%d missing from the report", i, i+1)
		}
	}
	if strings.Index(res.Content, "Seams") > strings.Index(res.Content, "line 3") {
		t.Error("the seam report must precede the transcript, or the output cap can cut it off")
	}
}

// TestTranscribeDenseImageOverlapsAndDedupes is the other half of the
// contract: an image with no blank row anywhere — a solid table, a terminal
// — has nowhere clean to cut, so the tool sends overlap, the chunks see the
// same strip twice, the merge removes the repeat, and the seam is flagged
// because a forced cut cannot be certified from the text alone.
func TestTranscribeDenseImageOverlapsAndDedupes(t *testing.T) {
	dense := pageLayout{width: 800, bandH: 1, gutterH: 0, bands: 4000} // ink on every row
	e, root := newTestExecutor(t)
	dense.writePNG(t, filepath.Join(root, "dense.png"))

	plan := planChunks(dense.render(), transcribeMaxChunksDefault)
	if len(plan) < 2 {
		t.Fatalf("a %dpx image should split, got %d chunks", dense.height(), len(plan))
	}
	for _, c := range plan[:len(plan)-1] {
		if c.overlap == 0 {
			t.Fatalf("chunk %d found a clean cut on an image with no blank rows", c.index+1)
		}
	}

	srv, callCount := transcribeServer(t, transcribeUsageJSON, func(n, total int) (string, error) {
		// The overlap means each chunk after the first sees, and
		// transcribes, the previous chunk's last line.
		var b strings.Builder
		if n > 1 {
			fmt.Fprintf(&b, "row %d\n", n*3-1)
		}
		for i := 0; i < 3; i++ {
			fmt.Fprintf(&b, "row %d\n", n*3+i)
		}
		return b.String(), nil
	})
	e.Gemini = reviewScreenshotClient(srv)

	res := runTool(t, e, "Transcribe", transcribeArgs{ImagePath: "dense.png"})
	if res.IsError {
		t.Fatalf("Transcribe failed: %s", res.Content)
	}
	for i := 3; i < 3*callCount()+3; i++ {
		if got := strings.Count(res.Content+"\n", fmt.Sprintf("row %d\n", i)); got != 1 {
			t.Errorf("row %d appears %d times after the merge, want exactly 1", i, got)
		}
	}
	if !strings.Contains(res.Content, "1 line removed as an exact repeat") {
		t.Errorf("the report does not record the removal:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "CHECK") {
		t.Errorf("a cut through content must be flagged for review:\n%s", res.Content)
	}
}

// TestTranscribeReportsSeamsForReview: the report has to say which boundaries
// were not clean. A merge that went wrong reads as ordinary prose, so the
// flag is the only thing standing between a dropped paragraph and a caller
// that believes the transcript.
func TestTranscribeReportsSeamsForReview(t *testing.T) {
	page := pageLayout{width: 800, bandH: 30, gutterH: 20, bands: 90}
	e, root := newTestExecutor(t)
	page.writePNG(t, filepath.Join(root, "page.png"))

	// Every chunk answers with unrelated text, so no seam can ever match.
	// With cuts landing in gutters no overlap is sent, so these come back
	// "not-needed" and clean — which is the honest answer.
	srv, _ := transcribeServer(t, transcribeUsageJSON, func(n, total int) (string, error) {
		return fmt.Sprintf("section %d body", n), nil
	})
	e.Gemini = reviewScreenshotClient(srv)

	res := runTool(t, e, "Transcribe", transcribeArgs{ImagePath: "page.png"})
	if res.IsError {
		t.Fatalf("Transcribe failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "Every cut landed in blank space") {
		t.Errorf("a page of clean gutter cuts should report every seam clean:\n%s", res.Content)
	}
	if strings.Contains(res.Content, "CHECK") {
		t.Errorf("nothing should be flagged when every cut was clean:\n%s", res.Content)
	}
}

// TestTranscribeFailedChunkReturnsNoPartialTranscript: a hole in the middle
// of a transcript is invisible, so a chunk that failed fails the call — but
// the chunks that succeeded were still billed and their cost must come home.
func TestTranscribeFailedChunkReturnsNoPartialTranscript(t *testing.T) {
	page := pageLayout{width: 800, bandH: 30, gutterH: 20, bands: 90}
	e, root := newTestExecutor(t)
	page.writePNG(t, filepath.Join(root, "page.png"))

	srv, _ := transcribeServer(t, transcribeUsageJSON, func(n, total int) (string, error) {
		if n == 2 {
			return "", fmt.Errorf("stub failure")
		}
		return fmt.Sprintf("section %d", n), nil
	})
	e.Gemini = reviewScreenshotClient(srv)

	res := runTool(t, e, "Transcribe", transcribeArgs{ImagePath: "page.png"})
	if !res.IsError {
		t.Fatalf("a failed chunk must fail the call, got:\n%s", res.Content)
	}
	if strings.Contains(res.Content, "section 1") || strings.Contains(res.Content, "section 3") {
		t.Errorf("a partial transcript leaked into the failure:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "chunk 2") {
		t.Errorf("the failure does not name which chunk failed:\n%s", res.Content)
	}
	if res.GeminiUsage == nil || res.GeminiUsage.Calls == 0 {
		t.Error("the chunks that succeeded were billed; their usage must still be reported")
	}
}

// TestTranscribeRegionReportsOriginalCoordinates: a y in the report is only
// useful if it can be handed to Crop against the file the caller named.
func TestTranscribeRegionReportsOriginalCoordinates(t *testing.T) {
	page := pageLayout{width: 800, bandH: 30, gutterH: 20, bands: 120}
	e, root := newTestExecutor(t)
	page.writePNG(t, filepath.Join(root, "page.png"))

	srv, callCount := transcribeServer(t, transcribeUsageJSON, func(n, total int) (string, error) {
		return fmt.Sprintf("section %d", n), nil
	})
	e.Gemini = reviewScreenshotClient(srv)

	const top = 2000
	res := runTool(t, e, "Transcribe", transcribeArgs{
		ImagePath: "page.png",
		Region:    fmt.Sprintf("0,%d,800,%d", top, page.height()),
	})
	if res.IsError {
		t.Fatalf("Transcribe failed: %s", res.Content)
	}
	if callCount() < 2 {
		t.Fatalf("the region should still have needed several chunks, got %d", callCount())
	}
	// Every chunk and seam position in the report must sit at or past the
	// region's own top, which only holds if the offset was applied.
	for _, line := range strings.Split(res.Content, "\n") {
		if !strings.Contains(line, " at y=") {
			continue
		}
		var after, before, y int
		if _, err := fmt.Sscanf(strings.TrimSpace(line), "%d\u2192%d at y=%d", &after, &before, &y); err != nil {
			continue
		}
		if y < top {
			t.Errorf("seam reported at y=%d, above the region's own top of %d — the offset was not applied", y, top)
		}
	}
}

// TestTranscribeWithoutAGeminiClient: the refusal has to name the reason, the
// way the other vision tools' does.
func TestTranscribeWithoutAGeminiClient(t *testing.T) {
	page := pageLayout{width: 800, bandH: 30, gutterH: 20, bands: 20}
	e, root := newTestExecutor(t)
	page.writePNG(t, filepath.Join(root, "page.png"))

	res := runTool(t, e, "Transcribe", transcribeArgs{ImagePath: "page.png"})
	if !res.IsError || !strings.Contains(res.Content, "no Gemini client") {
		t.Errorf("got %q, want a refusal naming the missing client", res.Content)
	}
}

func TestTranscribeRejectsAMissingFile(t *testing.T) {
	e, _ := newTestExecutor(t)
	res := runTool(t, e, "Transcribe", transcribeArgs{ImagePath: "nope.png"})
	if !res.IsError {
		t.Fatal("a missing file should be refused")
	}
}

// TestTranscribeSendsEveryChunkAtHighResolution: the split exists to buy
// resolution, and sending the later chunks at medium would hand most of it
// back.
func TestTranscribeSendsEveryChunkAtHighResolution(t *testing.T) {
	page := pageLayout{width: 800, bandH: 30, gutterH: 20, bands: 90}
	e, root := newTestExecutor(t)
	page.writePNG(t, filepath.Join(root, "page.png"))

	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, geminitest.Answer("text", transcribeUsageJSON))
	}))
	defer srv.Close()
	e.Gemini = reviewScreenshotClient(srv)

	if res := runTool(t, e, "Transcribe", transcribeArgs{ImagePath: "page.png"}); res.IsError {
		t.Fatalf("Transcribe failed: %s", res.Content)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) < 3 {
		t.Fatalf("only %d chunks went out", len(bodies))
	}
	for i, body := range bodies {
		if !strings.Contains(body, `"resolution":"`+gemini.ResolutionHigh+`"`) {
			t.Errorf("chunk %d was not sent at high resolution", i+1)
		}
	}
}
