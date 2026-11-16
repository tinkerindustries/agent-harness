package tools

import "github.com/mrgeoffrich/agent-harness/internal/wire"

// definitionsClaude is the array a Claude model is offered: Claude Code's
// own tools, under Claude Code's names and argument spellings, rather than
// the harness's canonical vocabulary the other arrays carry (docs/TOOLS.md,
// "Claude's array"). Each tool here is a canonical one the executor already
// runs; internal/toolvocab translates a call's name and arguments back into
// the canonical ones before the call reaches the log, so dispatch, the
// permission policy, deny patterns and every parent-facing frame see the
// same names they see on any other provider.
//
// It is written out rather than derived by subtraction, because the schemas
// are Claude Code's rather than the canonical ones renamed. What it leaves
// out is the harness's own additions Claude Code has no counterpart for —
// apply_patch, list, job_list and complete — and web_fetch, which Anthropic's
// server-side web_search and web_fetch replace (internal/anthropic appends
// them after this array, docs/ANTHROPIC-INTEGRATION.md). The six vision tools
// are absent for the reason every vision-capable model drops them: Claude
// reads images natively.
//
// Like every other array it is frozen and pinned by its own golden file
// (testdata/tools_claude.golden.json), and its descriptions name no limit
// that is a setting, for the same reason the canonical ones do not
// (docs/CACHE.md).
var definitionsClaude = []wire.Tool{
	function("Read", "Reads a file from the workspace. file_path is an absolute path or one relative to the workspace root. Returns the content with line numbers, cat -n style, starting at 1; offset and limit read part of a file too large to read at once. Reading a PNG, JPEG, or WebP file returns the image itself.", `{
		"type": "object",
		"properties": {
			"file_path": {"type": "string", "description": "The absolute or workspace-relative path to the file to read"},
			"offset": {"type": "integer", "description": "The line number to start reading from. Only provide if the file is too large to read at once"},
			"limit": {"type": "integer", "description": "The number of lines to read. Only provide if the file is too large to read at once"}
		},
		"required": ["file_path"]
	}`),
	function("Write", "Writes a file to the workspace, overwriting it if one already exists. Overwriting an existing file requires having read it with Read first in this session. Prefer Edit for changing part of an existing file.", `{
		"type": "object",
		"properties": {
			"file_path": {"type": "string", "description": "The absolute or workspace-relative path to the file to write"},
			"content": {"type": "string", "description": "The content to write to the file"}
		},
		"required": ["file_path", "content"]
	}`),
	function("Edit", "Performs an exact string replacement in a file. The file must have been read with Read in this session first. old_string must match the file's bytes exactly, without the line-number prefix Read shows, and must be unique in the file unless replace_all is set.", `{
		"type": "object",
		"properties": {
			"file_path": {"type": "string", "description": "The absolute or workspace-relative path to the file to modify"},
			"old_string": {"type": "string", "description": "The text to replace"},
			"new_string": {"type": "string", "description": "The text to replace it with (must be different from old_string)"},
			"replace_all": {"type": "boolean", "description": "Replace all occurrences of old_string (default false)"}
		},
		"required": ["file_path", "old_string", "new_string"]
	}`),
	function("Bash", "Executes a shell command in the workspace and returns its output once the command exits. Set run_in_background to return immediately with a task id instead: read what the command has produced with TaskOutput and stop it with TaskStop.", `{
		"type": "object",
		"properties": {
			"command": {"type": "string", "description": "The command to execute"},
			"timeout": {"type": "integer", "description": "Optional timeout in milliseconds. Omitted, the command gets the harness default, which is short; name a timeout for anything that builds, installs, packages or runs a test suite. A value above the harness ceiling is clamped down to the ceiling rather than rejected, so a larger number buys no more time. The message a timed-out command returns names the limit that actually applied. Work that cannot finish inside the ceiling has to be split into steps, or run in the background."},
			"description": {"type": "string", "description": "Clear, concise description of what this command does in 5-10 words"},
			"run_in_background": {"type": "boolean", "description": "Set to true to run this command in the background. Use TaskOutput to read the output later."}
		},
		"required": ["command"]
	}`),
	function("TaskOutput", "Retrieves output from a background task started by Bash with run_in_background: the output it has produced since the last time it was read, and whether it is still running. By default it waits for the task to finish first, bounded by timeout; set block to false to return at once.", `{
		"type": "object",
		"properties": {
			"task_id": {"type": "string", "description": "The task ID to get output from"},
			"block": {"type": "boolean", "description": "Whether to wait for completion (default true)"},
			"timeout": {"type": "integer", "description": "Max wait time in ms. A wait that runs out returns the output so far and leaves the task running."}
		},
		"required": ["task_id"]
	}`),
	function("TaskStop", "Stops a background task started by Bash with run_in_background. Stopping a task that has already finished is a no-op.", `{
		"type": "object",
		"properties": {
			"task_id": {"type": "string", "description": "The ID of the background task to stop"}
		},
		"required": ["task_id"]
	}`),
	function("Glob", "Fast file pattern matching. Supports glob patterns like \"**/*.js\" or \"src/**/*.ts\". Returns matching file paths.", `{
		"type": "object",
		"properties": {
			"pattern": {"type": "string", "description": "The glob pattern to match files against"},
			"path": {"type": "string", "description": "The directory to search in. Defaults to the workspace root."}
		},
		"required": ["pattern"]
	}`),
	function("Grep", "A search tool built on ripgrep. Supports full regular expression syntax; filter files with glob or type. Defaults to returning matching file paths only.", `{
		"type": "object",
		"properties": {
			"pattern": {"type": "string", "description": "The regular expression pattern to search for in file contents"},
			"path": {"type": "string", "description": "File or directory to search in. Defaults to the workspace root."},
			"glob": {"type": "string", "description": "Glob pattern to filter files (e.g. \"*.js\", \"*.{ts,tsx}\")"},
			"type": {"type": "string", "description": "File type to search, using ripgrep's own type names, e.g. js, py, go, rust. More efficient than glob for a standard file type; an unrecognised name is refused."},
			"output_mode": {"type": "string", "enum": ["files_with_matches", "content", "count"], "description": "files_with_matches (default), content, or count"},
			"-i": {"type": "boolean", "description": "Case insensitive search"},
			"-n": {"type": "boolean", "description": "Show line numbers in output. Requires output_mode: content, ignored otherwise. Defaults to true."},
			"-A": {"type": "integer", "description": "Number of lines to show after each match. Requires output_mode: content, ignored otherwise."},
			"-B": {"type": "integer", "description": "Number of lines to show before each match. Requires output_mode: content, ignored otherwise."},
			"-C": {"type": "integer", "description": "Number of lines to show before and after each match; takes precedence over -A and -B. Requires output_mode: content, ignored otherwise."},
			"multiline": {"type": "boolean", "description": "Enable multiline mode where . matches newlines and patterns can span lines. Default: false."},
			"head_limit": {"type": "integer", "description": "Limit output to the first N lines or entries — files for files_with_matches, lines for content, entries for count."}
		},
		"required": ["pattern"]
	}`),
	function("TaskCreate", "Creates one task in the working plan and returns the plan, with the new task's taskId. Call it once per task. State only; no side effects outside the session.", `{
		"type": "object",
		"properties": {
			"subject": {"type": "string", "description": "A brief title for the task, e.g. \"Run the test suite\""},
			"description": {"type": "string", "description": "A detailed description of what needs to be done"},
			"activeForm": {"type": "string", "description": "Present continuous form shown while the task is in progress, e.g. \"Running the test suite\""}
		},
		"required": ["subject", "description"]
	}`),
	function("TaskGet", "Retrieves one task from the working plan by its taskId.", `{
		"type": "object",
		"properties": {
			"taskId": {"type": "string", "description": "The ID of the task to retrieve"}
		},
		"required": ["taskId"]
	}`),
	function("TaskList", "Lists every task in the working plan, with its taskId, status and subject.", `{
		"type": "object",
		"properties": {}
	}`),
	function("TaskUpdate", "Updates one task in the working plan by taskId: its status, subject, description, or activeForm. Set status to \"deleted\" to remove the task. State only; no side effects outside the session.", `{
		"type": "object",
		"properties": {
			"taskId": {"type": "string", "description": "The ID of the task to update"},
			"status": {"type": "string", "enum": ["pending", "in_progress", "completed", "deleted"], "description": "New status for the task"},
			"subject": {"type": "string", "description": "New subject for the task"},
			"description": {"type": "string", "description": "New description for the task"},
			"activeForm": {"type": "string", "description": "Present continuous form shown while the task is in progress, e.g. \"Running the test suite\""}
		},
		"required": ["taskId"]
	}`),
	function("Agent", "Launches a subagent to handle a self-contained task in its own conversation. Only its final result joins this one.", `{
		"type": "object",
		"properties": {
			"description": {"type": "string", "description": "A short (3-5 word) description of the task"},
			"prompt": {"type": "string", "description": "The task for the agent to perform"},
			"subagent_type": {"type": "string", "description": "The type of specialized agent to use for this task. Defaults to a general-purpose agent."}
		},
		"required": ["description", "prompt"]
	}`),
}
