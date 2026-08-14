package tools

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mrgeoffrich/deepseek-harness/internal/settings"
)

// screenshotTimeout is the wall-clock bound on one capture, resolved through
// the settings registry so an operator can raise it for a slow application
// without a rebuild.
func (e *Executor) screenshotTimeout(ctx context.Context) time.Duration {
	if e.Timeouts.Screenshot > 0 {
		return e.Timeouts.Screenshot
	}
	if e.Settings != nil {
		if v, err := e.Settings.Duration(ctx, settings.KeyToolScreenshotTimeout); err == nil {
			return v
		}
	}
	return ScreenshotTimeout
}

// screenshotDriver is the Node script that drives the browser. It is embedded
// rather than shipped as a file on disk so a plain `go build` binary carries
// it (the same property internal/webassets gives the frontend), and written
// to a temporary path per call.
//
//go:embed screenshot.js
var screenshotDriver string

type screenshotArgs struct {
	URL             string             `json:"url"`
	Path            string             `json:"path"`
	Width           int                `json:"width"`
	Height          int                `json:"height"`
	Scale           int                `json:"device_scale_factor"`
	ColorScheme     string             `json:"color_scheme"`
	FullPage        bool               `json:"full_page"`
	Selector        string             `json:"selector"`
	WaitForSelector string             `json:"wait_for_selector"`
	WaitMS          int                `json:"wait_ms"`
	Actions         []screenshotAction `json:"actions"`
}

// screenshotAction is one step performed on the loaded page before the
// capture. The tool exists to photograph a page, and for most of a real
// frontend the state worth photographing is not the one a fresh load
// produces: a dialog has to be opened, an overlay dismissed, a field filled.
// Without these a session that needed any of that abandoned the tool
// entirely and drove Playwright through Bash instead — which is exactly the
// ad hoc invocation the tool was built to replace, so it went back to
// capturing full-page desktop light-scheme images with none of the standard
// this tool owns (docs/reviews/vision-path-2026-08-14.md).
//
// The vocabulary is deliberately small. These are the steps that get a page
// into a state, not a browser automation language: anything more expressive
// belongs in a script the session writes itself, and the moment this needs
// conditionals it has become one.
type screenshotAction struct {
	Type     string `json:"type"`
	Selector string `json:"selector"`
	Key      string `json:"key"`
	Value    string `json:"value"`
}

// The capture standard the tool owns. These are defaults rather than fixed
// values only where a caller has a real reason to differ; the bounds are what
// stop a call producing an image nothing downstream can use.
//
// The viewport is a desktop layout wide enough for a two-column page. Device
// scale stays at 1 by default: the image's next stop is usually
// ReviewScreenshot, which downscales it to roughly a thousand image tokens
// anyway, so doubling the pixels doubles the bytes against that tool's
// per-file cap and buys nothing in what the vision model sees. Raise it when
// a human is going to read fine detail in the transcript.
//
// full_page defaults off. A full-page capture of a long document is the
// single most common way to end up with an unreadable screenshot: it is
// downscaled to the same token budget as a viewport shot, so every control on
// it shrinks. The viewport shot, or a selector, is almost always the right
// answer (docs/TOOLS.md, "Screenshot").
const (
	screenshotDefaultWidth  = 1280
	screenshotDefaultHeight = 800
	screenshotMaxDimension  = 4000
	screenshotMinDimension  = 200
	screenshotDefaultScale  = 1
	screenshotMaxScale      = 3
	// screenshotMaxWaitMS bounds the explicit settle a caller can ask for, so
	// wait_ms cannot be used to hold a worker slot for the whole tool timeout.
	screenshotMaxWaitMS = 30_000
	// screenshotMaxActions bounds the pre-capture steps. A capture that needs
	// more than this is a workflow rather than a state to photograph, and the
	// cap is what keeps one call from spending the whole tool timeout on
	// per-action selector waits.
	screenshotMaxActions = 10
	// screenshotSettleTimeout is how long the driver waits for the network to
	// go idle after load before giving up and capturing anyway.
	screenshotSettleTimeoutMS = 3_000
)

// execScreenshot implements Screenshot: drive a headless Chromium to a URL
// and write one PNG or JPEG into the session's scratch directory
// (docs/TOOLS.md, "Screenshot").
//
// The harness owns the capture rather than leaving the agent to compose a
// Playwright invocation through Bash, because the things that decide whether
// a screenshot is worth anything — viewport, colour scheme, whether it is
// clipped to the element in question — are exactly the things an ad hoc
// invocation gets wrong or omits. What comes back is the file path plus what
// the page did while it was being captured: its title, whether the document
// was taller than the viewport, and any console or page errors, so a blank
// capture arrives with the reason it was blank.
func execScreenshot(ctx context.Context, e *Executor, argsRaw json.RawMessage) Result {
	var args screenshotArgs
	if err := json.Unmarshal(argsRaw, &args); err != nil {
		return errorResult("invalid arguments: %v", err)
	}
	if args.URL == "" {
		return errorResult("url is required")
	}
	if err := validateScreenshotURL(args.URL); err != nil {
		return errorResult("%v", err)
	}
	if args.Path == "" {
		return errorResult("path is required and must name the file to write")
	}

	path, err := resolveScreenshotOutput(e.Workspace, args.Path)
	if err != nil {
		return errorResult("%v", err)
	}
	cfg, err := screenshotConfig(args, path, e.screenshotTimeout(ctx))
	if err != nil {
		return errorResult("%v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return errorResult("create output directory: %v", err)
	}

	report, err := runScreenshotDriver(ctx, e.Workspace, cfg)
	if err != nil {
		return errorResult("%v", err)
	}
	if !report.OK {
		return errorResult("capture failed: %s", report.Error)
	}

	info, err := os.Stat(path)
	if err != nil {
		return errorResult("the driver reported success but wrote no file at %s: %v", args.Path, err)
	}
	return Result{Content: formatScreenshotReport(report, path, cfg, info.Size())}
}

// validateScreenshotURL keeps the browser on the schemes the tool is for.
// http and https reach a dev server the session started; file:// reaches a
// page it built. Everything else — data:, javascript:, chrome://, and the
// rest — is refused rather than handed to a browser that will do something
// with it.
func validateScreenshotURL(raw string) error {
	u, err := url.Parse(raw)
	// A bare host:port ("127.0.0.1:5173") is the common mistake here, and net/url
	// rejects it with "first path segment in URL cannot contain colon", which
	// says nothing about the fix. Both a parse failure and an absent scheme
	// get the same answer, because in practice both are the same mistake.
	if err != nil || u.Scheme == "" {
		return fmt.Errorf("url needs a scheme: pass an absolute http://, https://, or file:// URL, not %q", raw)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "file":
		return nil
	default:
		return fmt.Errorf("unsupported URL scheme %q: Screenshot captures http, https, and file URLs", u.Scheme)
	}
}

// screenshotOutputExtensions are the formats Chromium will encode. WebP is
// absent deliberately: ReviewScreenshot accepts one and the transcript
// endpoint serves one, but Playwright's screenshot writes PNG and JPEG only,
// so accepting .webp here would mean accepting an argument that cannot be
// honoured.
var screenshotOutputExtensions = map[string]bool{".png": true, ".jpg": true, ".jpeg": true}

// resolveScreenshotOutput confines the capture to the session's scratch
// directory.
//
// A screenshot is not part of a run's deliverable, and the system prompt
// already tells the model that files like it belong in scratch/
// (internal/session/prompt.go). Enforcing it here is what makes the tool safe
// to allow in a read-only session: the capture cannot land in a cloned
// repository, so it cannot turn into an unexplained diff, whatever the
// permission mode.
//
// The rule is where a relative path lands, not whether it is spelled right: a
// relative path that would fall outside scratch/ is taken as relative to
// scratch/ instead of refused. Refusing it was the earlier behaviour and it
// cost real sub-turns — a run capturing sixteen screens dropped the "scratch/"
// prefix on three separate batches, twelve calls in all, correcting itself
// each time and forgetting again after the next success (session
// sess-8df2a5f7, sub-turns 112, 113 and 179). Nothing was gained by the
// refusal: the confinement invariant is that the bytes land under scratch/,
// and joining the path there satisfies it exactly as the model's own
// "scratch/..." spelling does. The result line names the absolute path it
// wrote (formatScreenshotReport), so a relocated capture is not silent.
//
// An absolute path is left to stand or fail as given. It says where the file
// is meant to be, so re-rooting one under scratch/ would be overriding the
// caller rather than completing what they meant, and the refusal below is
// what tells them the tool cannot write there.
func resolveScreenshotOutput(workspace, userPath string) (string, error) {
	ext := strings.ToLower(filepath.Ext(userPath))
	if !screenshotOutputExtensions[ext] {
		return "", fmt.Errorf("path must end in .png, .jpg, or .jpeg, got %q", userPath)
	}
	path, err := ResolvePath(workspace, userPath)
	if err != nil {
		return "", err
	}
	scratch, err := ResolvePath(workspace, scratchDir)
	if err != nil {
		return "", fmt.Errorf("resolve scratch directory: %w", err)
	}
	if !withinRoot(scratch, path) && !filepath.IsAbs(userPath) {
		path, err = resolveScratchRelative(workspace, userPath)
		if err != nil {
			return "", err
		}
	}
	if !withinRoot(scratch, path) {
		return "", fmt.Errorf("screenshots are written to the scratch directory: pass a path under scratch/, not %q", userPath)
	}
	if path == scratch {
		return "", fmt.Errorf("path must name a file, not the scratch directory itself")
	}
	return path, nil
}

// screenshotDriverConfig is the JSON the driver script reads from argv. Field
// names are the script's, not the tool's: the tool's arguments are validated
// and defaulted into this shape first, so the script does no validation of
// its own.
type screenshotDriverConfig struct {
	URL                 string             `json:"url"`
	Path                string             `json:"path"`
	Width               int                `json:"width"`
	Height              int                `json:"height"`
	Scale               int                `json:"scale"`
	ColorScheme         string             `json:"colorScheme"`
	FullPage            bool               `json:"fullPage"`
	Selector            string             `json:"selector"`
	WaitForSelector     string             `json:"waitForSelector"`
	WaitMS              int                `json:"waitMs"`
	Actions             []screenshotAction `json:"actions"`
	ActionTimeoutMS     int                `json:"actionTimeoutMs"`
	NavigationTimeoutMS int                `json:"navigationTimeoutMs"`
	SettleTimeoutMS     int                `json:"settleTimeoutMs"`
}

// The action verbs the driver understands, and what each requires.
const (
	screenshotActionClick = "click"
	screenshotActionFill  = "fill"
	screenshotActionPress = "press"
	screenshotActionHover = "hover"
)

// validateScreenshotActions checks the steps before the browser launches, so
// a typo'd verb costs no browser start and names the set it should have come
// from. Selectors are not validated here — whether one matches is a fact
// about the page, and the driver reports it with the action's position.
func validateScreenshotActions(actions []screenshotAction) ([]screenshotAction, error) {
	if len(actions) > screenshotMaxActions {
		return nil, fmt.Errorf("at most %d actions, got %d: a capture needing more than that is a workflow, and belongs in a script", screenshotMaxActions, len(actions))
	}
	out := make([]screenshotAction, 0, len(actions))
	for i, a := range actions {
		a.Type = strings.ToLower(strings.TrimSpace(a.Type))
		a.Selector = strings.TrimSpace(a.Selector)
		a.Key = strings.TrimSpace(a.Key)
		switch a.Type {
		case screenshotActionClick, screenshotActionHover:
			if a.Selector == "" {
				return nil, fmt.Errorf("action %d (%s) needs a selector", i+1, a.Type)
			}
		case screenshotActionFill:
			if a.Selector == "" {
				return nil, fmt.Errorf("action %d (fill) needs a selector", i+1)
			}
		case screenshotActionPress:
			if a.Key == "" {
				return nil, fmt.Errorf("action %d (press) needs a key, for example \"Escape\" or \"Enter\"", i+1)
			}
		case "":
			return nil, fmt.Errorf("action %d has no type: each action needs one of click, fill, press, hover", i+1)
		default:
			return nil, fmt.Errorf("action %d has type %q: each action needs one of click, fill, press, hover", i+1, a.Type)
		}
		out = append(out, a)
	}
	return out, nil
}

func screenshotConfig(args screenshotArgs, path string, timeout time.Duration) (screenshotDriverConfig, error) {
	cfg := screenshotDriverConfig{
		URL:             args.URL,
		Path:            path,
		Width:           args.Width,
		Height:          args.Height,
		Scale:           args.Scale,
		ColorScheme:     strings.ToLower(strings.TrimSpace(args.ColorScheme)),
		FullPage:        args.FullPage,
		Selector:        strings.TrimSpace(args.Selector),
		WaitForSelector: strings.TrimSpace(args.WaitForSelector),
		WaitMS:          args.WaitMS,
		SettleTimeoutMS: screenshotSettleTimeoutMS,
	}

	if cfg.Width == 0 {
		cfg.Width = screenshotDefaultWidth
	}
	if cfg.Height == 0 {
		cfg.Height = screenshotDefaultHeight
	}
	if cfg.Width < screenshotMinDimension || cfg.Width > screenshotMaxDimension ||
		cfg.Height < screenshotMinDimension || cfg.Height > screenshotMaxDimension {
		return cfg, fmt.Errorf("width and height must each be between %d and %d pixels, got %dx%d",
			screenshotMinDimension, screenshotMaxDimension, cfg.Width, cfg.Height)
	}

	if cfg.Scale == 0 {
		cfg.Scale = screenshotDefaultScale
	}
	if cfg.Scale < 1 || cfg.Scale > screenshotMaxScale {
		return cfg, fmt.Errorf("device_scale_factor must be between 1 and %d, got %d", screenshotMaxScale, cfg.Scale)
	}

	switch cfg.ColorScheme {
	case "":
		cfg.ColorScheme = "light"
	case "light", "dark":
	default:
		return cfg, fmt.Errorf("color_scheme must be \"light\" or \"dark\", got %q", args.ColorScheme)
	}

	if cfg.WaitMS < 0 || cfg.WaitMS > screenshotMaxWaitMS {
		return cfg, fmt.Errorf("wait_ms must be between 0 and %d, got %d", screenshotMaxWaitMS, cfg.WaitMS)
	}

	// A selector clips the capture to one element, so full_page has nothing
	// to mean alongside it. Refusing the combination is better than silently
	// dropping one of the two, which would leave the model believing it got
	// something it did not ask for.
	if cfg.Selector != "" && cfg.FullPage {
		return cfg, errors.New("selector and full_page cannot both be set: a selector already clips the capture to one element")
	}

	actions, err := validateScreenshotActions(args.Actions)
	if err != nil {
		return cfg, err
	}
	cfg.Actions = actions

	// The driver's own navigation timeout sits inside the tool's wall-clock
	// timeout, so a slow page fails with the driver's message rather than
	// having the whole call killed with no output. The margin leaves room for
	// the browser launch and the process teardown.
	navigation := timeout - 10*time.Second
	if navigation < 5*time.Second {
		navigation = 5 * time.Second
	}
	cfg.NavigationTimeoutMS = int(navigation / time.Millisecond)

	// Actions share the navigation budget between them rather than each
	// getting it in full: ten steps at the navigation timeout apiece would
	// overrun the tool's wall clock and the call would be killed with no
	// output, which is the one failure mode that tells the model nothing.
	cfg.ActionTimeoutMS = cfg.NavigationTimeoutMS
	if n := len(cfg.Actions); n > 1 {
		cfg.ActionTimeoutMS = cfg.NavigationTimeoutMS / n
	}
	if cfg.ActionTimeoutMS < 1000 {
		cfg.ActionTimeoutMS = 1000
	}
	return cfg, nil
}

// screenshotReport is the driver's stdout, one JSON object.
type screenshotReport struct {
	OK             bool     `json:"ok"`
	Error          string   `json:"error"`
	URL            string   `json:"url"`
	Title          string   `json:"title"`
	Clipped        bool     `json:"clipped"`
	ActionsRun     []string `json:"actionsRun"`
	DocumentHeight int      `json:"documentHeight"`
	ViewportHeight int      `json:"viewportHeight"`
	ConsoleErrors  []string `json:"consoleErrors"`
	PageErrors     []string `json:"pageErrors"`
}

// runScreenshotDriver writes the embedded script to a temporary file and runs
// it under node. The script goes to the OS temp directory rather than the
// workspace: it is the harness's own machinery, and a session that listed its
// scratch directory should not find a file it did not put there.
func runScreenshotDriver(ctx context.Context, workspace string, cfg screenshotDriverConfig) (screenshotReport, error) {
	var report screenshotReport

	node, err := exec.LookPath("node")
	if err != nil {
		return report, errors.New("Screenshot is not available in this context: node is not on PATH")
	}

	script, err := os.CreateTemp("", "harness-screenshot-*.js")
	if err != nil {
		return report, fmt.Errorf("write driver script: %w", err)
	}
	defer os.Remove(script.Name())
	if _, err := script.WriteString(screenshotDriver); err != nil {
		script.Close()
		return report, fmt.Errorf("write driver script: %w", err)
	}
	if err := script.Close(); err != nil {
		return report, fmt.Errorf("write driver script: %w", err)
	}

	payload, err := json.Marshal(cfg)
	if err != nil {
		return report, fmt.Errorf("encode driver config: %w", err)
	}

	cmd := exec.CommandContext(ctx, node, script.Name(), string(payload))
	cmd.Dir = workspace
	// Chromium is a process tree, not one child. Reusing Bash's group killer
	// means a timeout reaches the browser rather than only the node process
	// that launched it, which would otherwise leave a headless Chromium
	// running for the rest of the session (docs/TOOLS.md, "Bash").
	group := bashGroup(cmd)
	cmd.WaitDelay = 2 * time.Second

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	if errors.Is(runErr, exec.ErrWaitDelay) {
		group.forceKill()
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return report, fmt.Errorf("capture timed out; the page may never finish loading. Try wait_for_selector for the element you care about, or a shorter page")
	}

	// The driver reports its own failures as a JSON object with ok:false and
	// exits non-zero, so a parseable stdout is preferred over the exit status
	// — it carries the message worth showing. Only an unparseable stdout
	// falls back to stderr, which is where a missing module or a browser that
	// will not launch shows up.
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &report); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		if detail == "" && runErr != nil {
			detail = runErr.Error()
		}
		trimmed, _ := truncate(detail, 4000)
		return report, fmt.Errorf("the browser driver failed to run: %s", trimmed)
	}
	return report, nil
}

// formatScreenshotReport is the tool result: what was written, then what the
// page did while it was written. The path comes first and in full, because it
// is the argument the model passes to ReviewScreenshot next.
func formatScreenshotReport(report screenshotReport, path string, cfg screenshotDriverConfig, size int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Wrote %s (%d bytes)\n", path, size)
	if cfg.Scale > 1 {
		fmt.Fprintf(&b, "Captured %dx%d at %dx scale, %s scheme", cfg.Width, cfg.Height, cfg.Scale, cfg.ColorScheme)
	} else {
		fmt.Fprintf(&b, "Captured %dx%d, %s scheme", cfg.Width, cfg.Height, cfg.ColorScheme)
	}
	switch {
	case report.Clipped:
		fmt.Fprintf(&b, ", clipped to %s", cfg.Selector)
	case cfg.FullPage:
		b.WriteString(", full page")
	}
	b.WriteString("\n")
	if report.Title != "" {
		fmt.Fprintf(&b, "Page title: %s\n", report.Title)
	}
	if report.URL != "" && report.URL != cfg.URL {
		fmt.Fprintf(&b, "Redirected to: %s\n", report.URL)
	}

	// What the page was put through before the shutter. A capture that looks
	// wrong is usually a state that was not reached, so the steps that did
	// run are the first thing worth knowing — and it confirms the capture is
	// of the state asked for rather than of the page as it loaded.
	if len(report.ActionsRun) > 0 {
		fmt.Fprintf(&b, "Before capturing, ran: %s\n", strings.Join(report.ActionsRun, ", "))
	}

	// The one fact a viewport capture cannot show is what it left out. Saying
	// so turns "the footer is missing" from a bug report into a known
	// consequence of the capture the model chose.
	if !cfg.FullPage && !report.Clipped && report.DocumentHeight > report.ViewportHeight && report.ViewportHeight > 0 {
		fmt.Fprintf(&b, "The document is %dpx tall and this captured the top %dpx. Scroll or set full_page to see the rest.\n",
			report.DocumentHeight, report.ViewportHeight)
	}

	// Console and page errors ride home with the image so a blank or broken
	// capture arrives with its explanation, rather than costing a second call
	// to go and find out.
	for _, line := range report.PageErrors {
		fmt.Fprintf(&b, "Uncaught page error: %s\n", line)
	}
	for _, line := range report.ConsoleErrors {
		fmt.Fprintf(&b, "Console error: %s\n", line)
	}
	return strings.TrimRight(b.String(), "\n")
}
