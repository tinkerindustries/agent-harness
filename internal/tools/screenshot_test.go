package tools

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// screenshotExecutor returns an executor over a workspace that has the
// scratch directory internal/workspace.Prepare creates for a real session,
// since that is where every capture has to land.
func screenshotExecutor(t *testing.T) (*Executor, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "scratch"), 0o755); err != nil {
		t.Fatal(err)
	}
	e, err := NewExecutor(root, &Policy{Mode: ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	return e, root
}

// The browser is only handed schemes the tool is for. A capture is not a
// general-purpose URL opener.
func TestScreenshotRejectsUnsupportedSchemes(t *testing.T) {
	cases := []struct {
		url  string
		want string
	}{
		{"data:text/html,<h1>hi</h1>", "unsupported URL scheme"},
		{"javascript:alert(1)", "unsupported URL scheme"},
		{"chrome://settings", "unsupported URL scheme"},
		// A bare host:port is the common mistake, and net/url's own complaint
		// about it says nothing the model can act on.
		{"127.0.0.1:5173", "url needs a scheme"},
		{"example.com/page", "url needs a scheme"},
	}
	for _, tc := range cases {
		if err := validateScreenshotURL(tc.url); err == nil {
			t.Errorf("validateScreenshotURL(%q) allowed it, want a refusal", tc.url)
		} else if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("validateScreenshotURL(%q) = %q, want it to mention %q", tc.url, err, tc.want)
		}
	}

	for _, ok := range []string{"http://127.0.0.1:5173/", "https://example.com/x?y=1", "file:///tmp/page.html", "HTTP://EXAMPLE.COM"} {
		if err := validateScreenshotURL(ok); err != nil {
			t.Errorf("validateScreenshotURL(%q) = %v, want it allowed", ok, err)
		}
	}
}

// The capture is confined to scratch/, which is what lets the tool run in a
// read-only session (internal/tools/policy.go): it can never leave a mark on
// a cloned repository.
func TestScreenshotConfinesOutputToScratch(t *testing.T) {
	_, root := screenshotExecutor(t)
	if err := os.MkdirAll(filepath.Join(root, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}

	refused := []struct {
		name string
		path string
		want string
	}{
		// An absolute path inside the workspace but outside scratch/ is the
		// one case still refused: it says where the file goes, and the tool
		// cannot write there.
		{"absolute path in a repository", filepath.Join(root, "repo", "shot.png"), "under scratch/"},
		{"escape from the workspace", "../shot.png", "escapes"},
		{"absolute path elsewhere", absElsewhere(t, "shot.png"), "escapes"},
		{"scratch itself", "scratch", "must end in .png"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveScratchImageOutput(root, tc.path)
			if err == nil {
				t.Fatalf("resolveScratchImageOutput(%q) allowed it, want a refusal", tc.path)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}

	for _, ok := range []string{"scratch/shot.png", "scratch/nested/dir/shot.jpeg", "scratch/a.JPG"} {
		if _, err := resolveScratchImageOutput(root, ok); err != nil {
			t.Errorf("resolveScratchImageOutput(%q) = %v, want it allowed", ok, err)
		}
	}
}

// A relative path is scratch-relative, however it is spelled. The prefix the
// schema asks for is what a caller should write, but dropping it costs a
// sub-turn per capture and buys nothing — so the path is relocated into
// scratch/ instead of refused, and the confinement invariant a read-only
// session depends on is the one that decides where it lands.
func TestScreenshotRelocatesRelativePathsIntoScratch(t *testing.T) {
	_, root := screenshotExecutor(t)
	if err := os.MkdirAll(filepath.Join(root, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The expectations are built from the symlink-resolved root, which is what
	// ResolvePath returns — on macOS a t.TempDir() sits under /var, itself a
	// link to /private/var.
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(resolvedRoot, "scratch")

	cases := []struct {
		name string
		path string
		want string
	}{
		{"workspace root", "shot.png", filepath.Join(scratch, "shot.png")},
		// The batch the live run kept getting wrong: a directory of captures
		// named without the prefix (session sess-8df2a5f7, sub-turn 179).
		{"nested directory", "after/01-session-list.png", filepath.Join(scratch, "after", "01-session-list.png")},
		{"a repository path", "repo/shot.png", filepath.Join(scratch, "repo", "shot.png")},
		// Traversal is cleaned by the join before it is resolved, so a path
		// that used to point out of scratch/ now lands inside it rather than
		// being refused. The invariant holds either way; only the answer to a
		// mis-spelled path changed.
		{"traversal out of scratch", "scratch/../repo/shot.png", filepath.Join(scratch, "repo", "shot.png")},
		// Already prefixed: resolved as workspace-relative, never doubled.
		{"already under scratch", "scratch/after/01.png", filepath.Join(scratch, "after", "01.png")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveScratchImageOutput(root, tc.path)
			if err != nil {
				t.Fatalf("resolveScratchImageOutput(%q) = %v, want it allowed", tc.path, err)
			}
			if got != tc.want {
				t.Fatalf("resolveScratchImageOutput(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// Playwright encodes PNG and JPEG only, so an extension it cannot honour is
// refused at the argument rather than producing a file with a lying name.
func TestScreenshotRejectsUnwritableFormats(t *testing.T) {
	_, root := screenshotExecutor(t)
	for _, path := range []string{"scratch/shot.webp", "scratch/shot.gif", "scratch/shot", "scratch/shot.png.txt"} {
		if _, err := resolveScratchImageOutput(root, path); err == nil {
			t.Errorf("resolveScratchImageOutput(%q) allowed it, want a refusal", path)
		}
	}
}

func TestScreenshotConfigDefaultsAndBounds(t *testing.T) {
	base := screenshotArgs{URL: "http://x/", Path: "scratch/a.png"}

	cfg, err := screenshotConfig(base, "/ws/scratch/a.png", ScreenshotTimeout)
	if err != nil {
		t.Fatalf("defaults rejected: %v", err)
	}
	if cfg.Width != screenshotDefaultWidth || cfg.Height != screenshotDefaultHeight {
		t.Errorf("viewport = %dx%d, want the desktop default", cfg.Width, cfg.Height)
	}
	if cfg.Scale != 1 {
		t.Errorf("scale = %d, want 1 — the image is downscaled by the vision model anyway", cfg.Scale)
	}
	if cfg.ColorScheme != "light" {
		t.Errorf("colorScheme = %q, want light", cfg.ColorScheme)
	}
	if cfg.FullPage {
		t.Error("full_page defaulted on; a full-page capture downscales every control until it is unreadable")
	}
	// The driver's navigation timeout has to sit inside the tool's wall-clock
	// timeout, so a slow page fails with a message instead of the whole call
	// being killed with no output.
	if got := int(ScreenshotTimeout.Milliseconds()); cfg.NavigationTimeoutMS >= got {
		t.Errorf("navigation timeout %dms is not inside the %dms tool timeout", cfg.NavigationTimeoutMS, got)
	}

	bad := []struct {
		name string
		args screenshotArgs
		want string
	}{
		{"width too small", screenshotArgs{URL: "http://x/", Width: 10}, "between"},
		{"width too large", screenshotArgs{URL: "http://x/", Width: 99999}, "between"},
		{"height too large", screenshotArgs{URL: "http://x/", Height: 99999}, "between"},
		{"scale too large", screenshotArgs{URL: "http://x/", Scale: 9}, "device_scale_factor"},
		{"scale negative", screenshotArgs{URL: "http://x/", Scale: -1}, "device_scale_factor"},
		{"unknown colour scheme", screenshotArgs{URL: "http://x/", ColorScheme: "sepia"}, "color_scheme"},
		{"wait too long", screenshotArgs{URL: "http://x/", WaitMS: 600_000}, "wait_ms"},
		// A selector already clips to one element, so honouring full_page
		// alongside it would mean silently ignoring one of the two.
		{"selector with full_page", screenshotArgs{URL: "http://x/", Selector: "#app", FullPage: true}, "cannot both be set"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := screenshotConfig(tc.args, "/ws/scratch/a.png", ScreenshotTimeout); err == nil {
				t.Fatal("accepted, want a refusal")
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestScreenshotRequiresUrlAndPath(t *testing.T) {
	e, _ := screenshotExecutor(t)

	res := runTool(t, e, "Screenshot", screenshotArgs{Path: "scratch/a.png"})
	if !res.IsError || !strings.Contains(res.Content, "url is required") {
		t.Errorf("missing url gave: %s", res.Content)
	}
	res = runTool(t, e, "Screenshot", screenshotArgs{URL: "http://127.0.0.1/"})
	if !res.IsError || !strings.Contains(res.Content, "path is required") {
		t.Errorf("missing path gave: %s", res.Content)
	}
}

// Argument validation happens before the browser is reached for, so a call
// with a bad argument gets the specific complaint even where node is absent —
// the same ordering Glance, Ground, and Detect use for their capability
// check.
func TestScreenshotValidatesArgumentsBeforeLaunchingTheBrowser(t *testing.T) {
	e, _ := screenshotExecutor(t)
	// An absolute path outside scratch/, since a relative one is no longer a
	// bad argument — it is relocated into scratch/ instead.
	res := runTool(t, e, "Screenshot", screenshotArgs{URL: "http://127.0.0.1/", Path: filepath.Join(e.Workspace, "repo", "a.png")})
	if !res.IsError {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(res.Content, "scratch") {
		t.Fatalf("error should name the scratch requirement, got: %s", res.Content)
	}
}

func TestFormatScreenshotReportNamesWhatTheCaptureLeftOut(t *testing.T) {
	cfg := screenshotDriverConfig{Width: 1280, Height: 800, Scale: 1, ColorScheme: "light", URL: "http://x/"}
	report := screenshotReport{
		OK: true, Title: "Session list", URL: "http://x/",
		DocumentHeight: 4200, ViewportHeight: 800,
	}
	out := formatScreenshotReport(report, "/ws/scratch/a.png", cfg, 51234)

	for _, want := range []string{"/ws/scratch/a.png", "51234 bytes", "1280x800", "Session list", "4200px", "top 800px"} {
		if !strings.Contains(out, want) {
			t.Errorf("report is missing %q:\n%s", want, out)
		}
	}

	// A full-page or element capture left nothing out, so the note must not
	// appear and send the model chasing content that is already in the image.
	cfg.FullPage = true
	if out := formatScreenshotReport(report, "/ws/scratch/a.png", cfg, 10); strings.Contains(out, "top 800px") {
		t.Errorf("full-page capture should not report truncation:\n%s", out)
	}
	cfg.FullPage = false
	cfg.Selector = "#app"
	report.Clipped = true
	if out := formatScreenshotReport(report, "/ws/scratch/a.png", cfg, 10); strings.Contains(out, "top 800px") {
		t.Errorf("clipped capture should not report truncation:\n%s", out)
	}
}

// A blank capture has to come back with the reason it was blank, or finding
// out costs another sub-turn.
func TestFormatScreenshotReportCarriesPageErrors(t *testing.T) {
	cfg := screenshotDriverConfig{Width: 390, Height: 844, Scale: 2, ColorScheme: "dark", URL: "http://x/"}
	report := screenshotReport{
		OK: true, URL: "http://x/redirected",
		ConsoleErrors: []string{"Failed to load resource: 404"},
		PageErrors:    []string{"TypeError: t.map is not a function"},
	}
	out := formatScreenshotReport(report, "/ws/scratch/a.png", cfg, 10)

	for _, want := range []string{"2x scale", "dark scheme", "Redirected to: http://x/redirected",
		"Uncaught page error: TypeError: t.map is not a function", "Console error: Failed to load resource: 404"} {
		if !strings.Contains(out, want) {
			t.Errorf("report is missing %q:\n%s", want, out)
		}
	}
}

// The end-to-end capture, against a local file so the test needs no network.
// Skipped where the browser stack is absent: the harness image has node and
// Playwright's Chromium, a developer's machine may not.
func TestScreenshotCapturesAPage(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not on PATH")
	}
	e, root := screenshotExecutor(t)

	page := filepath.Join(root, "scratch", "page.html")
	html := `<!doctype html><title>Capture test</title>
<style>body{margin:0;font-family:sans-serif} #tall{height:3000px} #box{width:200px;height:120px;background:#369}</style>
<div id="box">hello</div><div id="tall"></div>`
	if err := os.WriteFile(page, []byte(html), 0o644); err != nil {
		t.Fatal(err)
	}

	res := runTool(t, e, "Screenshot", screenshotArgs{
		URL:  fileURL(page),
		Path: "scratch/shot.png",
	})
	if res.IsError {
		if strings.Contains(res.Content, "browser driver failed to run") {
			t.Skipf("no usable Playwright browser here: %s", res.Content)
		}
		t.Fatalf("capture failed: %s", res.Content)
	}

	info, err := os.Stat(filepath.Join(root, "scratch", "shot.png"))
	if err != nil {
		t.Fatalf("no image written: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("image is empty")
	}
	if !strings.Contains(res.Content, "Capture test") {
		t.Errorf("result should carry the page title, got: %s", res.Content)
	}
	// The document is 3000px of filler against an 800px viewport, so the
	// result has to say the capture stopped short.
	if !strings.Contains(res.Content, "Scroll or set full_page") {
		t.Errorf("result should say the viewport cut the document off, got: %s", res.Content)
	}

	// A selector clips to the element, which is the whole reason this drives
	// the library rather than the playwright CLI.
	res = runTool(t, e, "Screenshot", screenshotArgs{
		URL: fileURL(page), Path: "scratch/box.png", Selector: "#box",
	})
	if res.IsError {
		t.Fatalf("clipped capture failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "clipped to #box") {
		t.Errorf("result should say it clipped, got: %s", res.Content)
	}
	clipped, err := os.Stat(filepath.Join(root, "scratch", "box.png"))
	if err != nil {
		t.Fatalf("no clipped image written: %v", err)
	}
	if clipped.Size() >= info.Size() {
		t.Errorf("the 200x120 element capture (%d bytes) is not smaller than the full viewport (%d bytes)", clipped.Size(), info.Size())
	}
}

// A selector that matches nothing must fail with the selector named, not with
// a Playwright stack trace.
func TestScreenshotReportsAMissingSelector(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not on PATH")
	}
	e, root := screenshotExecutor(t)
	page := filepath.Join(root, "scratch", "page.html")
	if err := os.WriteFile(page, []byte("<!doctype html><title>t</title><p>hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	e.Timeouts.Screenshot = 20 * 1e9 // 20s: the wait for a missing selector is bounded by this
	res := runTool(t, e, "Screenshot", screenshotArgs{
		URL: fileURL(page), Path: "scratch/x.png", Selector: "#nope",
	})
	if !res.IsError {
		t.Fatal("expected a failure for a selector that matches nothing")
	}
	if strings.Contains(res.Content, "browser driver failed to run") {
		t.Skipf("no usable Playwright browser here: %s", res.Content)
	}
	if !strings.Contains(res.Content, "#nope") {
		t.Errorf("error should name the selector, got: %s", res.Content)
	}
}

// The driver config is what the script parses, so its JSON has to stay in the
// shape screenshot.js reads.
func TestScreenshotDriverConfigMarshalsTheScriptsFieldNames(t *testing.T) {
	cfg, err := screenshotConfig(screenshotArgs{URL: "http://x/", Selector: "#app"}, "/ws/scratch/a.png", ScreenshotTimeout)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"url", "path", "width", "height", "scale", "colorScheme", "fullPage",
		"selector", "waitForSelector", "waitMs", "navigationTimeoutMs", "settleTimeoutMs"} {
		if _, ok := got[key]; !ok {
			t.Errorf("driver config is missing %q, which screenshot.js reads", key)
		}
	}
}

// TestScreenshotActionValidation pins the checks that happen before the
// browser launches, so a malformed step costs no browser start and names both
// its position and the set it should have come from.
func TestScreenshotActionValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		actions []screenshotAction
		want    string
	}{
		{"unknown verb", []screenshotAction{{Type: "scroll", Selector: "#x"}}, "action 1 has type \"scroll\""},
		{"no verb", []screenshotAction{{Selector: "#x"}}, "action 1 has no type"},
		{"click without a selector", []screenshotAction{{Type: "click"}}, "action 1 (click) needs a selector"},
		{"fill without a selector", []screenshotAction{{Type: "fill", Value: "hi"}}, "action 1 (fill) needs a selector"},
		{"press without a key", []screenshotAction{{Type: "click", Selector: "#a"}, {Type: "press"}}, "action 2 (press) needs a key"},
		{"too many", make([]screenshotAction, screenshotMaxActions+1), "at most 10 actions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := screenshotConfig(screenshotArgs{URL: "http://x/", Actions: tc.actions}, "/ws/scratch/a.png", ScreenshotTimeout)
			if err == nil {
				t.Fatalf("expected a refusal for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}

	// A press with no selector is legal: the key goes to the page.
	cfg, err := screenshotConfig(screenshotArgs{
		URL:     "http://x/",
		Actions: []screenshotAction{{Type: "press", Key: "Escape"}, {Type: "Click", Selector: " #open "}},
	}, "/ws/scratch/a.png", ScreenshotTimeout)
	if err != nil {
		t.Fatalf("valid actions were refused: %v", err)
	}
	// Verbs are matched case-insensitively and selectors are trimmed, so a
	// model's whitespace does not become a selector that matches nothing.
	if cfg.Actions[1].Type != "click" || cfg.Actions[1].Selector != "#open" {
		t.Errorf("action was not normalised: %+v", cfg.Actions[1])
	}
	// Ten steps each waiting the full navigation timeout would overrun the
	// tool's wall clock and be killed with no output, so the budget is shared.
	if cfg.ActionTimeoutMS <= 0 || cfg.ActionTimeoutMS > cfg.NavigationTimeoutMS {
		t.Errorf("action timeout %d should sit inside the navigation timeout %d", cfg.ActionTimeoutMS, cfg.NavigationTimeoutMS)
	}
}

// TestScreenshotRunsActionsBeforeCapturing is the end-to-end case the actions
// exist for: a control that only appears once something has been clicked. A
// session that could not do this abandoned the tool and drove Playwright
// through Bash instead (docs/reviews/vision-path-2026-08-14.md).
func TestScreenshotRunsActionsBeforeCapturing(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not on PATH")
	}
	e, root := screenshotExecutor(t)

	page := filepath.Join(root, "scratch", "page.html")
	html := `<!doctype html><title>Actions test</title>
<style>body{margin:0}#panel{display:none;width:300px;height:200px;background:#264}</style>
<button id="open" onclick="document.getElementById('panel').style.display='block'">Open</button>
<input id="name"><div id="panel">now visible</div>`
	if err := os.WriteFile(page, []byte(html), 0o644); err != nil {
		t.Fatal(err)
	}

	// #panel is display:none on load, so clipping to it can only succeed if
	// the click ran first — which makes the capture itself the assertion.
	res := runTool(t, e, "Screenshot", screenshotArgs{
		URL:      fileURL(page),
		Path:     "scratch/panel.png",
		Selector: "#panel",
		Actions: []screenshotAction{
			{Type: "fill", Selector: "#name", Value: "geoff"},
			{Type: "click", Selector: "#open"},
		},
	})
	if res.IsError {
		if strings.Contains(res.Content, "browser driver failed to run") {
			t.Skipf("no usable Playwright browser here: %s", res.Content)
		}
		t.Fatalf("capture with actions failed: %s", res.Content)
	}
	if _, err := os.Stat(filepath.Join(root, "scratch", "panel.png")); err != nil {
		t.Fatalf("no image written: %v", err)
	}
	// The result says what the page was put through, so a capture of the
	// wrong state can be traced to the step that did or did not run.
	if !strings.Contains(res.Content, "Before capturing, ran: fill #name, click #open") {
		t.Errorf("result should list the actions it ran, got: %s", res.Content)
	}
}

// A step whose selector never appears fails the call and names the step. The
// alternative — capturing anyway — produces a screenshot of the wrong state
// that nothing downstream can identify as wrong.
func TestScreenshotFailsOnAnActionThatCannotRun(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not on PATH")
	}
	e, root := screenshotExecutor(t)
	page := filepath.Join(root, "scratch", "page.html")
	if err := os.WriteFile(page, []byte("<!doctype html><title>t</title><p>hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	e.Timeouts.Screenshot = 20 * 1e9
	res := runTool(t, e, "Screenshot", screenshotArgs{
		URL: fileURL(page), Path: "scratch/x.png",
		Actions: []screenshotAction{{Type: "click", Selector: "#missing"}},
	})
	if !res.IsError {
		t.Fatal("expected a failure for a step that cannot run")
	}
	if strings.Contains(res.Content, "browser driver failed to run") {
		t.Skipf("no usable Playwright browser here: %s", res.Content)
	}
	if !strings.Contains(res.Content, "action 1 (click #missing) failed") {
		t.Errorf("error should name the step by position and intent, got: %s", res.Content)
	}
	if _, err := os.Stat(filepath.Join(root, "scratch", "x.png")); err == nil {
		t.Error("a failed action must not leave a capture of the wrong state behind")
	}
}
