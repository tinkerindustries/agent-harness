"""Find the local script files a shell command runs, for the autonomy guard (docs/plans/autonomy-guard.md).

A script counts when the command hands a file to an interpreter (python3 x.py, node x.mjs, bash x.sh,
uv run x.py, npx tsx x.ts), runs it by path (./run.sh, .scratch/run.sh), or sources it (source x, . x).
Inline code (python3 - <<EOF, -c, -e) is already visible in the command and is not counted. Modules
(python3 -m pytest) and compiled binaries are not scripts.
"""
import re
import shlex

from guard_rules import strip_heredocs

_SPLIT = re.compile(r"&&|\|\||;|\||\n|\$\(|`|\(|\)")
_ENV = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*=")
_WRAPPERS = {"time", "nohup", "exec", "env", "command", "caffeinate"}
_INTERPRETER = re.compile(r"(?:^|/)(?:python(?:\d+(?:\.\d+)?)?|node|bash|sh|zsh|ruby|perl|deno|bun|tsx|ts-node)$")
_SCRIPT_SUFFIX = re.compile(r"\.(?:py|mjs|cjs|js|ts|mts|cts|sh|bash|zsh|rb|pl)$")
_INLINE = {"-c", "-e", "--eval", "-p", "--print", "-"}
_VALUE_OPTIONS = {"--import", "--require", "-r", "--loader", "-W", "-X"}


def _words(segment):
    try:
        return shlex.split(segment, comments=False)
    except ValueError:
        return segment.split()


def _first_file(words):
    """The first non-option word after an interpreter, or None for inline code and modules."""
    skip = False
    for word in words:
        if skip:
            skip = False
            continue
        if word in _INLINE or word == "-m":
            return None
        if word in _VALUE_OPTIONS:
            skip = True
            continue
        if word.startswith("-"):
            continue
        return word
    return None


def _script_in(words):
    while words and (_ENV.match(words[0]) or words[0] in _WRAPPERS):
        words = words[1:]
        if words and words[0].lstrip("-").isdigit():
            words = words[1:]  # caffeinate -t 3, timeout 30
        while words and words[0].startswith("-") and words[0] != "-":
            words = words[1:]
    if not words:
        return None
    head, rest = words[0], words[1:]
    if head in ("source", "."):
        return rest[0] if rest else None
    if head == "uv" and rest[:1] == ["run"]:
        rest = [w for w in rest[1:] if not w.startswith("-")]
        return _script_in(rest) if rest and _INTERPRETER.search(rest[0]) else (rest[0] if rest and _SCRIPT_SUFFIX.search(rest[0]) else None)
    if head in ("npx", "pnpx") or (head in ("pnpm", "yarn") and rest[:1] == ["exec"]):
        rest = [w for w in (rest[1:] if head in ("pnpm", "yarn") else rest) if not w.startswith("-")]
        return _script_in(rest) if rest else None
    if head in ("pnpm", "yarn") and rest[:1] in (["tsx"], ["node"]):
        return _script_in(rest)
    if _INTERPRETER.search(head):
        target = _first_file(rest)
        return target if target and ("/" in target or _SCRIPT_SUFFIX.search(target)) else None
    if ("/" in head) and _SCRIPT_SUFFIX.search(head):
        return head
    return None


def executed_scripts(command):
    """Script paths the command runs, as written, in order, without repeats."""
    found = []
    for segment in _SPLIT.split(strip_heredocs(command)):
        segment = segment.strip()
        if segment:
            script = _script_in(_words(segment))
            if script and script not in found:
                found.append(script)
    return found


def escalation_reasons(command, scripts_run=()):
    """Why the command should skip the stage-1 model and go straight to stage 2; empty when it need not.

    A command escalates when it runs two or more local scripts and at least one of them was written or
    changed in this session, or when it runs a session script whose content is unknown or too long to screen.
    Scripts that already existed in the project do not count on their own.
    """
    session = [entry for entry in scripts_run or ()
               if entry.get("origin") in ("written_this_session", "changed_this_session")]
    reasons = []
    if len(executed_scripts(command)) >= 2 and session:
        reasons.append("runs_several_scripts")
    if any(entry.get("content") is None for entry in session):
        reasons.append("unscreened_script")
    return reasons
