"""Resolve the package.json scripts a shell command runs, for the autonomy guard (docs/plans/autonomy-guard.md).

`pnpm release` shows the model one word. package.json says what it runs (`node scripts/release.mjs`), and that
file's header says it uploads a build. This module finds the npm, pnpm, yarn and bun script invocations in a
command, reads package.json for each, follows scripts that call other scripts, and attaches the head of each
local script file the script line runs. It only reads files and never runs anything.

An entry has runner, script, command (the package.json line, or None when package.json has no such script) and
files (path, excerpt and truncated for each local script the line runs). Inline code is not read.
"""
import json
import re
import shlex
from pathlib import Path

from guard_rules import strip_heredocs, strip_quoted
from script_detection import _ENV, _SPLIT, _WRAPPERS, executed_scripts

RUNNERS = ("npm", "pnpm", "yarn", "bun")
MAX_DEPTH = 3
MAX_ENTRIES = 8
EXCERPT_CHARS = 1500
FALLBACK_CHARS = 600

_RUN = {"run", "run-script"}
_NPM_SCRIPT_NAMES = {"test", "start", "stop", "restart"}
_BUILTINS = {
    "add", "audit", "bin", "cache", "ci", "config", "create", "dedupe", "dlx", "doctor", "exec", "fetch", "i",
    "import", "info", "init", "install", "install-test", "link", "list", "ln", "login", "logout", "ls",
    "outdated", "pack", "patch", "prune", "publish", "rebuild", "remove", "rm", "root", "run", "run-script",
    "self-update", "server", "setup", "store", "uninstall", "unlink", "up", "update", "upgrade", "view", "why",
    "x", "workspaces", "whoami", "version", "help", "link", "unlink", "env", "licenses",
}
_DIRECTORY_OPTIONS = {"-C", "--dir", "--prefix", "--cwd"}
_VALUE_OPTIONS = {"--filter", "-F", "-w", "--workspace", "--registry", "--loglevel", "--reporter"}
_WORKTREE = "/.claude/worktrees/"


def _strip_prefix(words):
    while words and (_ENV.match(words[0]) or words[0] in _WRAPPERS):
        words = words[1:]
        if words and words[0].lstrip("-").isdigit():
            words = words[1:]
        while words and words[0].startswith("-") and words[0] != "-":
            words = words[1:]
    return words


def _words(segment):
    try:
        return shlex.split(segment, comments=False)
    except ValueError:
        return segment.split()


def _call_in(words):
    """(runner, script, directory option) when the words run a package script, else None."""
    words = _strip_prefix(words)
    if len(words) < 2 or words[0] not in RUNNERS:
        return None
    runner, rest = words[0], words[1:]
    directory = None
    while rest and rest[0].startswith("-"):
        option = rest[0]
        if "=" in option and option.split("=", 1)[0] in _DIRECTORY_OPTIONS:
            directory = option.split("=", 1)[1]
            rest = rest[1:]
        elif option in _DIRECTORY_OPTIONS and len(rest) > 1:
            directory, rest = rest[1], rest[2:]
        elif option in _VALUE_OPTIONS and len(rest) > 1:
            rest = rest[2:]
        else:
            rest = rest[1:]
    if not rest:
        return None
    if rest[0] in _RUN:
        rest = [word for word in rest[1:] if not word.startswith("-")]
        return (runner, rest[0], directory) if rest else None
    if runner == "npm":
        return (runner, rest[0], directory) if rest[0] in _NPM_SCRIPT_NAMES else None
    if rest[0] in _BUILTINS or rest[0].startswith(("./", "/", "@")):
        return None
    return runner, rest[0], directory


def package_script_calls(command):
    """(runner, script, directory) for each package script the command runs, in order, without repeats.

    The directory is the last `cd` target or `-C`/`--prefix` value before the call, as written, or None.
    """
    calls, cwd = [], None
    for segment in _SPLIT.split(strip_quoted(strip_heredocs(command))):
        words = _words(segment.strip())
        if not words:
            continue
        if words[0] == "cd" and len(words) > 1:
            cwd = words[1]
            continue
        call = _call_in(words)
        if call:
            runner, script, directory = call
            where = directory if directory is not None else cwd
            if directory is not None and cwd is not None and not Path(directory).is_absolute():
                where = str(Path(cwd) / directory)
            if (runner, script, where) not in calls:
                calls.append((runner, script, where))
    return calls


def _locate_directory(working_directory, where, repositories):
    """The directory a call runs in, falling back to the repository when its worktree is gone."""
    base = Path(working_directory)
    path = base if where is None else (Path(where) if Path(where).is_absolute() else base / where)
    if path.is_dir():
        return path
    text = str(path)
    if _WORKTREE in text:
        root, rest = text.split(_WORKTREE, 1)
        remainder = rest.split("/", 1)[1] if "/" in rest else ""
        candidate = Path(root) / remainder
        if candidate.is_dir():
            return candidate
    for repository in repositories:
        if text.startswith(str(repository)) and Path(repository).is_dir():
            return Path(repository)
    return None


def _package_json(directory):
    for folder in [directory, *directory.parents]:
        candidate = folder / "package.json"
        if candidate.is_file():
            try:
                return folder, json.loads(candidate.read_text(encoding="utf-8")).get("scripts") or {}
            except (OSError, ValueError):
                return folder, {}
        if folder == folder.parent:
            break
    return None, {}


def _leading_comment(text):
    lines = text.splitlines()
    if lines and lines[0].startswith("#!"):
        lines = lines[1:]
    head, in_block = [], False
    for line in lines:
        stripped = line.strip()
        if in_block:
            head.append(line)
            if "*/" in stripped or stripped.endswith(('"""', "'''")):
                in_block = False
            continue
        if not head and not stripped:
            continue
        if stripped.startswith(("/*", '"""', "'''")):
            head.append(line)
            closed = stripped.count("*/") or (stripped.count('"""') >= 2 or stripped.count("'''") >= 2)
            in_block = not closed
        elif stripped.startswith(("//", "#")):
            head.append(line)
        else:
            break
    return "\n".join(head)


def excerpt(path):
    """The head of a script file: its leading comment block, else the first lines, capped. None if unreadable."""
    try:
        text = path.read_text(encoding="utf-8")
    except (OSError, UnicodeDecodeError):
        return None, False
    head = _leading_comment(text)
    limit = EXCERPT_CHARS if head else FALLBACK_CHARS
    head = head or text
    shown = head[:limit]
    if len(head) > limit and "\n" in shown:
        shown = shown[:shown.rindex("\n")]
    shown = shown.rstrip()
    return shown, len(text.rstrip()) > len(shown)


def _files(command, folder, shown):
    found = []
    for name in executed_scripts(command):
        path = (folder / name).resolve() if not Path(name).is_absolute() else Path(name)
        if path in shown:
            continue
        shown.add(path)
        text, truncated = excerpt(path)
        found.append({"path": name, "excerpt": text, "truncated": truncated})
    return found


def resolve(command, working_directory, repositories=()):
    """Package scripts the command runs (and the scripts those call), resolved from the checkout on disk."""
    entries, seen, shown = [], set(), set()

    def visit(runner, script, where, depth):
        if len(entries) >= MAX_ENTRIES:
            return
        directory = _locate_directory(working_directory, where, repositories)
        folder, scripts = _package_json(directory) if directory else (None, {})
        key = (str(folder), script)
        if key in seen:
            return
        seen.add(key)
        line = scripts.get(script)
        entry = {"runner": runner, "script": script, "command": line, "files": []}
        entries.append(entry)
        if runner in ("npm", "yarn"):
            for hook in ("pre" + script, "post" + script):
                if hook in scripts:
                    visit(runner, hook, str(folder), depth)
        if line is None or folder is None:
            return
        entry["files"] = _files(line, folder, shown)
        if depth < MAX_DEPTH:
            for nested_runner, nested, nested_where in package_script_calls(line):
                visit(nested_runner, nested, nested_where or str(folder), depth + 1)

    for runner, script, where in package_script_calls(command):
        visit(runner, script, where, 1)
    return entries
