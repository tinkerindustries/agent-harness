package tools

import (
	"encoding/json"

	"github.com/mrgeoffrich/deepseek-harness/internal/deepseek"
)

// definitions is the fixed, ordered tool array sent on every request in
// every session and every permission mode. Order and content never vary by
// mode or by request — that is what keeps the shared prefix stable
// (docs/CACHE.md). Schemas are non-strict: optional arguments are simply
// absent from "required" rather than encoded as anyOf-null, matching the
// trained-in shape the target harnesses use (docs/TOOLS.md).
var definitions = []deepseek.Tool{
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
	function("TodoWrite", "Replace the working plan with the given list of todos. State only; no side effects outside the session.", `{
		"type": "object",
		"properties": {
			"todos": {
				"type": "array",
				"description": "The full current todo list, replacing whatever was there before",
				"items": {
					"type": "object",
					"properties": {
						"content": {"type": "string", "description": "Imperative task description, e.g. \"Run the test suite\""},
						"status": {"type": "string", "enum": ["pending", "in_progress", "completed"]},
						"activeForm": {"type": "string", "description": "Present-continuous form shown while in progress, e.g. \"Running the test suite\""}
					},
					"required": ["content", "status", "activeForm"]
				}
			}
		},
		"required": ["todos"]
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
	function("ReviewScreenshot", "Send one to four screenshots to Google Gemini's vision model and return its diagnosis of the question. The first image gets high resolution and the rest medium, so put the screenshot that needs the closest scrutiny first. Screenshots are PNG, JPEG, or WebP files, workspace-relative or absolute paths, at most 5 MB each. Returns Gemini's findings as the tool result.", `{
		"type": "object",
		"properties": {
			"image_paths": {"type": "array", "items": {"type": "string"}, "description": "Absolute or workspace-relative paths to PNG, JPEG, or WebP screenshot files. At most 4, each at most 5 MB. The first image is reviewed at high resolution and the rest at medium, so put the screenshot you care about most first."},
			"question": {"type": "string", "description": "What to diagnose about the screenshots"},
			"spec": {"type": "string", "description": "Optional design spec or target CSS to compare the screenshots against"}
		},
		"required": ["image_paths", "question"]
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

func function(name, description, parameters string) deepseek.Tool {
	return deepseek.Tool{
		Type: "function",
		Function: deepseek.ToolFunction{
			Name:        name,
			Description: description,
			Parameters:  json.RawMessage(parameters),
		},
	}
}

// Definitions returns the fixed tool array. Callers must not mutate it.
func Definitions() []deepseek.Tool {
	return definitions
}
