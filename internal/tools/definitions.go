package tools

import (
	"encoding/json"

	"github.com/mrgeoffrich/deepseek-harness/internal/provider"
	"github.com/mrgeoffrich/deepseek-harness/internal/wire"
)

// The tool array is per-provider (docs/KIMI-INTEGRATION.md §4.5, decision 5):
// DeepSeek gets all sixteen tools, and Kimi K3 gets the fourteen that remain
// once the two vision tools are dropped — K3 reads images natively, so
// ReviewScreenshot (the Gemini round-trip) and Screenshot (capture-and-
// describe) are both redundant for it (docs/TOOLS.md). A provider's array is
// fixed and ordered, shared by every session on that provider in every
// permission mode; it never varies by mode or by request — that is what keeps
// the shared prefix stable (docs/CACHE.md). Each array is pinned by its own
// golden file (internal/tools/testdata/tools_*.golden.json, asserted by
// TestToolArrayGolden), the same byte-stability guard the single array used
// to have, now that there are two frozen request heads
// (docs/KIMI-INTEGRATION.md decision 6).
//
// Schemas are non-strict: optional arguments are simply absent from
// "required" rather than encoded as anyOf-null, matching the trained-in shape
// the target harnesses use (docs/TOOLS.md).
var definitionsDeepSeek = []wire.Tool{
	function("Read", "Read a file from the workspace. Returns content with line numbers, cat -n style.", `{
		"type": "object",
		"properties": {
			"file_path": {"type": "string", "description": "Absolute or workspace-relative path to the file to read"},
			"offset": {"type": "integer", "description": "Line number to start reading from, 1-based"},
			"limit": {"type": "integer", "description": "Maximum number of lines to read"}
		},
		"required": ["file_path"]
	}`),
	function("Write", "Create a new file or overwrite an existing one. Overwriting a file requires having read it first in this session.", `{
		"type": "object",
		"properties": {
			"file_path": {"type": "string", "description": "Absolute or workspace-relative path to write"},
			"content": {"type": "string", "description": "Full content to write to the file"}
		},
		"required": ["file_path", "content"]
	}`),
	function("Edit", "Replace an exact string in a file with another. old_string must match exactly once unless replace_all is set. Requires having read the file first in this session.", `{
		"type": "object",
		"properties": {
			"file_path": {"type": "string", "description": "Absolute or workspace-relative path to the file to edit"},
			"old_string": {"type": "string", "description": "Exact text to replace, without any line-number prefix"},
			"new_string": {"type": "string", "description": "Text to replace it with"},
			"replace_all": {"type": "boolean", "description": "Replace every occurrence instead of requiring a unique match"}
		},
		"required": ["file_path", "old_string", "new_string"]
	}`),
	function("Bash", "Run a shell command in the workspace. Foreground only; output is captured and returned once the command exits.", `{
		"type": "object",
		"properties": {
			"command": {"type": "string", "description": "The shell command to run"},
			"timeout": {"type": "integer", "description": "Maximum time to allow the command to run, in milliseconds"},
			"description": {"type": "string", "description": "A short human-readable description of what the command does"}
		},
		"required": ["command"]
	}`),
	function("Glob", "Find files by path pattern, e.g. **/*.go.", `{
		"type": "object",
		"properties": {
			"pattern": {"type": "string", "description": "Glob pattern to match"},
			"path": {"type": "string", "description": "Directory to search from; defaults to the workspace root"}
		},
		"required": ["pattern"]
	}`),
	function("Grep", "Search file contents by regular expression. Defaults to returning matching file paths only.", `{
		"type": "object",
		"properties": {
			"pattern": {"type": "string", "description": "Regular expression to search for"},
			"path": {"type": "string", "description": "File or directory to search; defaults to the workspace root"},
			"glob": {"type": "string", "description": "Glob to filter which files are searched, e.g. *.go"},
			"output_mode": {"type": "string", "enum": ["files_with_matches", "content", "count"], "description": "files_with_matches (default), content, or count"}
		},
		"required": ["pattern"]
	}`),
	function("List", "List the entries of a directory.", `{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "Directory to list"},
			"ignore": {"type": "array", "items": {"type": "string"}, "description": "Glob patterns to exclude"}
		},
		"required": ["path"]
	}`),
	function("TaskCreate", "Add one or more tasks to the working plan. Takes an array so seeding a whole plan is still one call; each task is minted its own taskId for later TaskGet/TaskList/TaskUpdate calls. State only; no side effects outside the session.", `{
		"type": "object",
		"properties": {
			"tasks": {
				"type": "array",
				"description": "The tasks to add, in plan order",
				"items": {
					"type": "object",
					"properties": {
						"subject": {"type": "string", "description": "A brief, actionable title, e.g. \"Run the test suite\""},
						"description": {"type": "string", "description": "What needs to be done"},
						"activeForm": {"type": "string", "description": "Present-continuous form shown while in progress, e.g. \"Running the test suite\""},
						"status": {"type": "string", "enum": ["pending", "in_progress", "completed"], "description": "Defaults to pending"}
					},
					"required": ["subject", "description", "activeForm"]
				}
			}
		},
		"required": ["tasks"]
	}`),
	function("TaskGet", "Fetch one task from the working plan by taskId.", `{
		"type": "object",
		"properties": {
			"taskId": {"type": "string", "description": "The taskId to fetch"}
		},
		"required": ["taskId"]
	}`),
	function("TaskList", "List tasks in the working plan, optionally filtered by status. A fully optional call: no arguments lists the whole plan.", `{
		"type": "object",
		"properties": {
			"status": {"type": "string", "enum": ["pending", "in_progress", "completed"], "description": "Only list tasks with this status; omit for all"}
		}
	}`),
	function("TaskUpdate", "Patch one task's status, subject, description, or activeForm by taskId, or remove it with status \"deleted\". A small, targeted call keeps the plan current without rewriting the whole list. State only; no side effects outside the session.", `{
		"type": "object",
		"properties": {
			"taskId": {"type": "string", "description": "The taskId to update or delete"},
			"status": {"type": "string", "enum": ["pending", "in_progress", "completed", "deleted"], "description": "pending, in_progress, or completed patch the task; deleted removes it"},
			"subject": {"type": "string", "description": "A brief, actionable title, e.g. \"Run the test suite\""},
			"description": {"type": "string", "description": "What needs to be done"},
			"activeForm": {"type": "string", "description": "Present-continuous form shown while in progress, e.g. \"Running the test suite\""}
		},
		"required": ["taskId"]
	}`),
	function("Task", "Delegate a self-contained task to a subagent. The subagent runs in its own conversation; only its final result joins this one.", `{
		"type": "object",
		"properties": {
			"description": {"type": "string", "description": "A short, 3-5 word summary of the task"},
			"prompt": {"type": "string", "description": "The full task for the subagent to perform"},
			"subagent_type": {"type": "string", "description": "Which subagent to delegate to"}
		},
		"required": ["description", "prompt", "subagent_type"]
	}`),
	function("WebFetch", "Fetch a URL and answer a question against its content.", `{
		"type": "object",
		"properties": {
			"url": {"type": "string", "description": "The URL to fetch"},
			"prompt": {"type": "string", "description": "What to extract or answer from the fetched page"}
		},
		"required": ["url", "prompt"]
	}`),
	// The ReviewScreenshot description names no limits. The image count and
	// per-file size caps are settings (tools.reviewscreenshot_max_images,
	// tools.reviewscreenshot_max_bytes), and this text is part of the frozen
	// request head (docs/CACHE.md): a number here would make the head vary
	// per installation and change under operators' feet, costing the whole
	// prompt cache. The model discovers a bound from the refusal message,
	// which states the actual limit — the same shape the other tools' caps
	// use.
	function("ReviewScreenshot", "Send the screenshots to Google Gemini's vision model, which can see them where you cannot. Every answer opens with what the model says is actually on the screen, so a result with no findings still tells you the page rendered and what it rendered — read that line before deciding whether to ask again. Pass the design spec, target CSS, or mock markup in spec whenever one exists: it is the only standard the reviewer is told to judge against, and without it deliberate design choices get reported as breakage. The first image gets high resolution and the rest medium, so put the screenshot that needs the closest scrutiny first — and when the question is about one small control, screenshot that element rather than the whole page, because a full-page capture is downscaled until small detail is unreadable. Screenshots are PNG, JPEG, or WebP files, workspace-relative or absolute paths. A call costs many times what an ordinary sub-turn costs, and the result says what it cost, so ask one well-aimed question rather than re-asking a screenshot the reviewer has already answered on. A call without conversation_id starts a new conversation and its result carries the id; passing that id continues it, with the earlier questions and answers re-sent — send new image_paths with it when you have re-captured the page, and they replace the images under discussion while the thread survives.", `{
		"type": "object",
		"properties": {
			"image_paths": {"type": "array", "items": {"type": "string"}, "description": "Absolute or workspace-relative paths to PNG, JPEG, or WebP screenshot files. The first image is reviewed at high resolution and the rest at medium, so put the screenshot you care about most first. Capture the element itself rather than the whole page when the question is about a small control. On a follow-up, omit these to keep discussing the same images, or pass new ones to swap in a fresh capture."},
			"question": {"type": "string", "description": "What to diagnose about the screenshots — name the area you are unsure about rather than listing points to confirm, which turns the review into a checklist and misses what you did not think to ask."},
			"spec": {"type": "string", "description": "The design spec, target CSS, or mock markup to judge the screenshots against — the only standard the reviewer treats as authoritative. Supply it whenever one exists: without it the review falls back to defects visible on their own terms, and anything intentional but unconventional reads as a bug. Fixed when the conversation starts; omit on a follow-up. Ignored in describe mode, which judges nothing."},
			"mode": {"type": "string", "enum": ["review", "describe"], "description": "review, the default, judges the screenshots and reports what is wrong with them. describe makes no judgement: it returns what is on the screen — the layout, and every element's text, role and styling — which is what to ask for when you need to know what a page actually shows rather than whether it is correct. Fixed when the conversation starts."},
			"conversation_id": {"type": "string", "description": "Id from an earlier ReviewScreenshot call. Passing it continues that conversation: the earlier questions and answers are re-sent along with your new question, so \"look closer at the header\" costs one call rather than a fresh review. Omit it to start a new conversation."}
		},
		"required": ["image_paths", "question"]
	}`),
	// Like ReviewScreenshot's, this description names no limits: the timeout
	// is a setting and the dimension bounds are stated by the refusal message
	// that quotes them, so nothing here varies per installation
	// (docs/CACHE.md).
	function("Screenshot", "Capture a web page as an image with a headless browser, writing one PNG or JPEG into scratch/ for ReviewScreenshot to review or a human to look at in the transcript. Defaults to the visible viewport in the light colour scheme, which is almost always what you want: a full-page capture of a long document is downscaled to the same size as a viewport one, so everything on it shrinks until small controls are unreadable. When the question is about one control, pass selector and capture just that element. When the page has a dark mode, capture both colour schemes — a layout that holds in one can break in the other. When the state worth photographing is not the one a fresh load produces — a dialog that has to be opened, an overlay covering the form, a field that has to be filled — pass actions to drive the page there first, rather than dropping to a browser script and losing these defaults. The result reports the file written, which actions ran, whether the document ran past the bottom of the viewport, and any console or page errors, so a blank capture comes back with the reason it was blank.", `{
		"type": "object",
		"properties": {
			"url": {"type": "string", "description": "The http, https, or file URL to capture. A dev server this session started is the usual target."},
			"path": {"type": "string", "description": "Where to write the image, under scratch/ — for example scratch/home-dark.png. Must end in .png, .jpg, or .jpeg. Name it for what it shows, because this path is what you pass to ReviewScreenshot and what a human sees under the image in the transcript."},
			"width": {"type": "integer", "description": "Viewport width in pixels. Defaults to a desktop layout; pass a phone width to check a responsive layout."},
			"height": {"type": "integer", "description": "Viewport height in pixels."},
			"device_scale_factor": {"type": "integer", "description": "Pixel density, 1 by default. Raise it only when fine detail has to stay readable at full size — it multiplies the file size, and a vision model downscales the image regardless."},
			"color_scheme": {"type": "string", "enum": ["light", "dark"], "description": "The colour scheme to emulate. Defaults to light."},
			"full_page": {"type": "boolean", "description": "Capture the whole scrollable document instead of the viewport. Off by default, and usually the wrong choice: prefer a selector, or a taller viewport."},
			"selector": {"type": "string", "description": "A CSS selector to clip the capture to, so one control fills the image instead of being a few pixels of a whole page. Cannot be combined with full_page."},
			"wait_for_selector": {"type": "string", "description": "A CSS selector to wait for before capturing — the way to photograph content that arrives after the initial render. Runs before actions, so it is how you say the page is ready for them."},
			"wait_ms": {"type": "integer", "description": "Milliseconds to wait after the page settles and any actions have run, for an animation or transition to finish."},
			"actions": {"type": "array", "description": "Steps to perform on the loaded page before capturing, in order — the way to photograph a state a fresh page load does not produce. Each is one of: {\"type\": \"click\", \"selector\": \"...\"}, {\"type\": \"hover\", \"selector\": \"...\"}, {\"type\": \"fill\", \"selector\": \"...\", \"value\": \"...\"}, or {\"type\": \"press\", \"key\": \"Escape\"} with an optional selector to press within. A step whose selector never appears fails the whole call and names the step, rather than quietly capturing the wrong state.", "items": {
				"type": "object",
				"properties": {
					"type": {"type": "string", "enum": ["click", "fill", "press", "hover"], "description": "What to do."},
					"selector": {"type": "string", "description": "The element to act on. Required for click, hover and fill; optional for press, which goes to the page when omitted."},
					"value": {"type": "string", "description": "Text to type, for fill."},
					"key": {"type": "string", "description": "The key to press, for press — for example Escape, Enter, or Tab."}
				},
				"required": ["type"]
			}}
		},
		"required": ["url", "path"]
	}`),
	function("Complete", "End the run and report its outcome. summary is prose for a human; result is the machine-readable payload, validated against the schema given in the opening message if one was supplied.", `{
		"type": "object",
		"properties": {
			"summary": {"type": "string", "description": "Prose summary of what was done, for a human reading the transcript"},
			"result": {"description": "The machine-readable result payload"},
			"status": {"type": "string", "enum": ["done", "gave_up"], "description": "Whether the task finished or the model is stopping without finishing it"}
		},
		"required": ["summary"]
	}`),
}

// definitionsKimi is Kimi K3's array: DeepSeek's sixteen minus the two tools
// that exist only because DeepSeek cannot see images. Building it by
// subtraction states that relationship and cannot drift from it — if a tool
// is added to DeepSeek's array, Kimi's changes the same way unless it is
// named here. The result is pinned by its own golden file like DeepSeek's.
var definitionsKimi = without(definitionsDeepSeek, "Screenshot", "ReviewScreenshot")

// without returns tools minus every entry whose name is in drop. Callers
// must not mutate the result.
func without(tools []wire.Tool, drop ...string) []wire.Tool {
	out := make([]wire.Tool, 0, len(tools))
	for _, t := range tools {
		keep := true
		for _, d := range drop {
			if t.Function.Name == d {
				keep = false
				break
			}
		}
		if keep {
			out = append(out, t)
		}
	}
	return out
}

func function(name, description, parameters string) wire.Tool {
	return wire.Tool{
		Type: "function",
		Function: wire.ToolFunction{
			Name:        name,
			Description: description,
			Parameters:  json.RawMessage(parameters),
		},
	}
}

// Definitions returns the DeepSeek tool array — the sixteen tools the
// harness has always shipped, unchanged byte for byte
// (docs/KIMI-INTEGRATION.md §4.2). It is the array every pre-Phase-8 caller
// meant, and the default for anything that does not resolve to a provider
// (an unknown model falls back to the default provider, DeepSeek). Callers
// must not mutate it.
func Definitions() []wire.Tool {
	return definitionsDeepSeek
}

// DefinitionsFor returns the frozen tool array for the provider serving
// model, resolved through the one model→provider table
// (internal/provider, docs/KIMI-INTEGRATION.md §4.3). Queue validation
// rejects an unknown model before a session is created, so the fallback to
// the DeepSeek array below is belt-and-braces for direct CLI callers, not a
// route anything can take by mistake. Callers must not mutate the result.
func DefinitionsFor(model string) []wire.Tool {
	p, err := provider.ModelFor(model)
	if err != nil {
		return definitionsDeepSeek
	}
	return DefinitionsForProvider(p)
}

// DefinitionsForProvider returns the frozen tool array for one provider.
// Callers must not mutate the result.
func DefinitionsForProvider(p provider.Name) []wire.Tool {
	switch p {
	case provider.Kimi:
		return definitionsKimi
	default:
		return definitionsDeepSeek
	}
}
