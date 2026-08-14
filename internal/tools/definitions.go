package tools

import (
	"encoding/json"

	"github.com/mrgeoffrich/deepseek-harness/internal/promptvariant"
	"github.com/mrgeoffrich/deepseek-harness/internal/provider"
	"github.com/mrgeoffrich/deepseek-harness/internal/wire"
)

// The tool array is per-provider (docs/KIMI-INTEGRATION.md §4.5, decision 5):
// DeepSeek gets all twenty tools, and Kimi K3 gets the fourteen that remain
// once the six vision tools are dropped — K3 reads images natively, so
// Glance, Ground, Detect, Transcribe, Crop (the Gemini round-trips and the
// local image operation that feeds them) and Screenshot (capture-and-describe)
// are all redundant for it (docs/TOOLS.md). A provider's array is fixed and ordered,
// shared by every session on that provider in every permission mode; it
// never varies by mode or by request — that is what keeps the shared prefix
// stable (docs/CACHE.md). Each array is pinned by its own golden file
// (internal/tools/testdata/tools_*.golden.json, asserted by
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
	// The four vision tools' descriptions name no limits. The image count and
	// per-file size caps are settings (tools.reviewscreenshot_max_images,
	// tools.reviewscreenshot_max_bytes — vision.go keeps the original setting
	// keys, docs/VISION-TOOLKIT.md §5 decision), and this text is part of the
	// frozen request head (docs/CACHE.md): a number here would make the head
	// vary per installation and change under operators' feet, costing the
	// whole prompt cache. The model discovers a bound from the refusal
	// message, which states the actual limit — the same shape the other
	// tools' caps use.
	//
	// Glance is the general, prose-answering tool — what ReviewScreenshot and
	// AskVision both were, now one tool with no imposed findings shape. Ground
	// and Detect exist because prose is the wrong return type for "where is
	// it": they hand back pixel boxes a caller can act on, feed to Crop for a
	// closer look, or drop straight into Glance's region to ask about just
	// that spot (docs/VISION-TOOLKIT.md §4, §6).
	function("Glance", "Ask Google Gemini's vision model about one or more images: describe them, ask a question in your own words, or transcribe the visible text verbatim. Use this for anything the answer to which is prose — comparing two captures, reading text off a chart, checking what a screenshot shows. When what you actually need is where something is, Ground or Detect return a pixel box instead of a description of one. query and ocr are mutually exclusive: query sends exactly the question you write, ocr transcribes text line by line without judging it, and omitting both describes the image in general. Put any spec, CSS, or context in query ahead of the question you ask, and say what standard you are judging against — a vision model given none falls back on general web convention and reports deliberate choices as breakage. region crops the image to a pixel box before sending it, so a spot Ground or Detect just located can be looked at closer and sharper than re-describing the whole page; it works with exactly one image. The first image goes at high resolution and the rest at medium, so put the one the question is really about first. A call without conversation_id starts a conversation and its result carries the id; passing that id continues it, so \"look closer at the header\" costs one call rather than a whole fresh look. Images are PNG, JPEG, or WebP, workspace-relative or absolute paths. A call costs many times an ordinary sub-turn and the result says what it cost.", `{
		"type": "object",
		"properties": {
			"image_paths": {"type": "array", "items": {"type": "string"}, "description": "Absolute or workspace-relative paths to PNG, JPEG, or WebP files. The first is read at high resolution and the rest at medium, so put the image the question is really about first."},
			"query": {"type": "string", "description": "The question to ask about the image(s), in your own words, sent as you wrote it. Mutually exclusive with ocr. Naming the images helps when there is more than one — they arrive labelled Image 1, Image 2 in the order you list them."},
			"ocr": {"type": "boolean", "description": "Transcribe every piece of visible text verbatim — titles, body text, labels, watermarks — instead of describing the image or answering a question. Mutually exclusive with query."},
			"ocr_extra": {"type": "string", "description": "Additional requirements for the transcription, appended after the base instruction. Only meaningful alongside ocr."},
			"conversation_id": {"type": "string", "description": "Id from an earlier Glance call, to ask a follow-up about the same images. The earlier questions and answers are re-sent with your new one, so \"look closer at the header\" costs one call rather than a fresh description, and image_paths becomes optional — omit it to keep discussing the same images, or pass new ones to swap in a re-capture while the thread survives. Omit conversation_id entirely to start fresh; the result of every call carries the id to continue it."},
			"region": {"type": "string", "description": "Crop to this pixel box, X1,Y1,X2,Y2, before sending the image — the way to look closer at a spot Ground or Detect already located. Only valid with exactly one image path."}
		},
		"required": ["image_paths"]
	}`),
	function("Ground", "Locate something in an image and get back its pixel box, not a description of roughly where it is. Say what to find in target, described by what distinguishes it — its visible text, its position, the block it sits in — because the tool matches the words you gave it rather than the intent behind them: ask for \"the main heading\" on a page whose largest text is a nav brand and the nav brand is what comes back. Several numbered matches means the description fitted more than one element; narrow it and ask again rather than picking one. Every match comes back with a coarse position (top-left, center, bottom-right, and the rest of that grid) and its box as x1,y1,x2,y2 in the image's own pixels. Use the box to Crop a tight second look at full resolution, or pass it as Glance's region to ask a question about just that spot instead of the whole page. Returns \"no match found\" rather than guessing when nothing does. region here narrows the search to part of the image first — useful when you already know roughly where to look and want a tighter answer — and the returned box is still in the original image's coordinates either way. Images are PNG, JPEG, or WebP, workspace-relative or absolute paths. A call costs many times an ordinary sub-turn and the result says what it cost.", `{
		"type": "object",
		"properties": {
			"image_path": {"type": "string", "description": "Absolute or workspace-relative path to a PNG, JPEG, or WebP image."},
			"target": {"type": "string", "description": "What to locate, in your own words — a labelled control, a region of the page, anything visible in the image. Every match is returned, not just the first."},
			"region": {"type": "string", "description": "Search only this pixel box, X1,Y1,X2,Y2, instead of the whole image. Returned boxes are still reported in the original image's coordinates."}
		},
		"required": ["image_path", "target"]
	}`),
	function("Detect", "Inventory every instance of a kind of thing in an image and get back a numbered list of pixel boxes, one per match — for \"what buttons are on this page\" rather than \"where is the submit button\", which is what Ground answers. category names the kind to look for and is worth naming precisely: the model enumerates what LOOKS like the category, so a status badge shaped like a button is listed as a button, which makes an inventory a starting point to check against the markup rather than an authority on what the page holds. Omitted, category defaults to UI elements generally (buttons, links, inputs, icons, labels, headings, images, badges), and every entry's label carries the element's own visible text. Feed a box from the result to Crop for a full-resolution look at one entry, or to Glance's region to ask a question about just it. region narrows the search to part of the image first, the returned boxes staying in the original image's coordinates regardless. Images are PNG, JPEG, or WebP, workspace-relative or absolute paths. A call costs many times an ordinary sub-turn and the result says what it cost.", `{
		"type": "object",
		"properties": {
			"image_path": {"type": "string", "description": "Absolute or workspace-relative path to a PNG, JPEG, or WebP image."},
			"category": {"type": "string", "description": "The kind of thing to inventory. Defaults to UI elements generally (buttons, links, inputs, icons, labels, headings, images, badges); name a narrower category to cut the list down to what you actually care about."},
			"region": {"type": "string", "description": "Search only this pixel box, X1,Y1,X2,Y2, instead of the whole image. Returned boxes are still reported in the original image's coordinates."}
		},
		"required": ["image_path"]
	}`),
	function("Crop", "Cut a pixel box out of an image into its own file. No model call and no cost — this is local image manipulation, the way to turn a box Ground or Detect located into an image Glance can read at full resolution instead of as a few pixels of the whole page. region is the box to cut, the same x1,y1,x2,y2 shape Ground and Detect report. output names where to write the result, defaulting to the source's own name with .crop.png appended, next to it; scale enlarges the cut region by an integer factor, for a small source where a plain crop would still be too small to read clearly.", `{
		"type": "object",
		"properties": {
			"image_path": {"type": "string", "description": "Absolute or workspace-relative path to the PNG, JPEG, or WebP image to cut from."},
			"region": {"type": "string", "description": "The pixel box to cut out, X1,Y1,X2,Y2 — the same shape Ground and Detect report, so a match either tool located can be cut out directly."},
			"output": {"type": "string", "description": "Where to write the cropped image, under scratch/ — crops go where screenshots go, never beside a file in a cloned repository. Defaults to the source's name with .crop.png appended. A relative path lands under scratch/ whether or not you spell the prefix; an absolute path outside it is refused. The extension chooses the encoding; one this harness cannot encode falls back to the source's own format."},
			"scale": {"type": "integer", "description": "Enlarge the cropped region by this integer factor before writing it, for a source small enough that a plain crop would still be hard to read."}
		},
		"required": ["image_path", "region"]
	}`),
	// Transcribe is the one vision tool that makes more than one call, and
	// its description says so in words rather than numbers for the same
	// reason as the rest: the chunk cap and the concurrency are settings,
	// and a figure here would vary per installation inside the frozen
	// request head (docs/CACHE.md).
	function("Transcribe", "Read all the text off an image too tall for one look — a full-page screenshot, a long document, a scrolling capture. It cuts the image into horizontal chunks along bands of blank pixels so no line of text is sliced, transcribes each chunk in its own vision call at full resolution, and joins the results back into one document. Use it instead of Glance with ocr whenever the image is much taller than it is wide: a vision model spends the same fixed budget on an image whatever its size, so a tall page read in one call comes back with the detail of a thumbnail, and the transcript quietly covers only the part it could still resolve. The result opens with a report — where the image was cut, what was removed as a repeat at each join, and which joins to check against the image — and then the transcript. Read that report: a merge that goes wrong drops or duplicates a line silently, so a seam marked CHECK is the one place the text may not match the page, and the y positions it names can be handed straight to Crop for a closer look. Costs one vision call per chunk, all of them billed together, and the result says what the lot cost. Images are PNG, JPEG, or WebP, workspace-relative or absolute paths.", `{
		"type": "object",
		"properties": {
			"image_path": {"type": "string", "description": "Absolute or workspace-relative path to the PNG, JPEG, or WebP image to transcribe."},
			"region": {"type": "string", "description": "Transcribe only this pixel box, X1,Y1,X2,Y2, instead of the whole image — the way to read one column or one section of a long page without paying for the rest. Reported cut positions stay in the original image's coordinates."}
		},
		"required": ["image_path"]
	}`),
	// Like the vision tools' descriptions, this one names no limits: the
	// timeout is a setting and the dimension bounds are stated by the
	// refusal message that quotes them, so nothing here varies per
	// installation (docs/CACHE.md).
	function("Screenshot", "Capture a web page as an image with a headless browser, writing one PNG or JPEG into scratch/ for Glance to review or a human to look at in the transcript. Defaults to the visible viewport in the light colour scheme, which is almost always what you want: a full-page capture of a long document is downscaled to the same size as a viewport one, so everything on it shrinks until small controls are unreadable. When the question is about one control, pass selector and capture just that element. When the page has a dark mode, capture both colour schemes — a layout that holds in one can break in the other. When the state worth photographing is not the one a fresh load produces — a dialog that has to be opened, an overlay covering the form, a field that has to be filled — pass actions to drive the page there first, rather than dropping to a browser script and losing these defaults. The result reports the file written, which actions ran, whether the document ran past the bottom of the viewport, and any console or page errors, so a blank capture comes back with the reason it was blank.", `{
		"type": "object",
		"properties": {
			"url": {"type": "string", "description": "The http, https, or file URL to capture. A dev server this session started is the usual target."},
			"path": {"type": "string", "description": "Where to write the image, under scratch/ — for example scratch/home-dark.png. Must end in .png, .jpg, or .jpeg. Name it for what it shows, because this path is what you pass to Glance and what a human sees under the image in the transcript."},
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

// definitionsKimi is Kimi K3's array: DeepSeek's twenty minus the six
// tools that exist only because DeepSeek cannot see images. Building it by
// subtraction states that relationship and cannot drift from it — if a tool
// is added to DeepSeek's array, Kimi's changes the same way unless it is
// named here. The result is pinned by its own golden file like DeepSeek's.
var definitionsKimi = without(definitionsDeepSeek, "Screenshot", "Glance", "Ground", "Detect", "Transcribe", "Crop")

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

// Definitions returns the DeepSeek tool array — the twenty tools the
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

// DefinitionsForVariant resolves the tool array for the provider serving
// model with the named variant's tool subtractions applied
// (internal/promptvariant.ToolsDroppedBy): the provider's frozen array,
// minus every tool the variant drops, in the same order. A variant that
// drops nothing — the shipped prompt, every wording variant — returns the
// frozen array itself, and the head it renders is the provider's shipped
// head byte for byte (TestPromptGolden). Subtraction only: the `without`
// helper can only remove tools, so a variant can never make a session's
// array carry a tool that does not exist.
//
// The session resolves its array through this function wherever it needs
// one — the schema stored on the session row (internal/session/runner.go)
// and every request's tool list (internal/session/turn.go) — so a
// tool-dropping variant's array and the head rendered from it can never
// disagree. The variant name rides the session row
// (store.Session.PromptVariant), and a resumed session resolves through it
// too (internal/session/resume.go), so the array a variant session is sent
// is the same before and after a resume.
func DefinitionsForVariant(model, variant string) []wire.Tool {
	defs := DefinitionsFor(model)
	dropped := promptvariant.ToolsDroppedBy(variant)
	if len(dropped) == 0 {
		return defs
	}
	return without(defs, dropped...)
}
