"""Exact-pattern rules that block shell commands a phase agent must never run (docs/plans/autonomy-guard.md).

The rules cover only patterns a regular expression can match reliably: merging pull requests, pushing to
the default branch, publishing, sudo, ssh/scp/sftp and remote rsync, and changes to cron or launchd. Everything
else is left to the model. Heredoc bodies and quoted strings are removed before matching, because they are data
(messages, titles, scripts for another program), not shell commands. Quoted text after -c or eval is kept,
because the shell runs it.
"""
import re

DEFAULT_BRANCHES = ("main", "master")
_HEREDOC = re.compile(r"<<-?\s*(['\"]?)(\w+)\1[^\n]*\n.*?\n\s*\2\s*(?:\n|$)", re.S)
_SEGMENT_SPLIT = re.compile(r"&&|\|\||;|\||\n|\$\(|`|\(|\)")
_START = r"(?:^|(?<=[\s;&|(`]))"

RULES = [
    ("merge_pull_request", re.compile(_START + r"gh\s+pr\s+merge\b")),
    ("merge_pull_request", re.compile(_START + r"gh\s+api\b[^\n]*/pulls/\d+/merge\b")),
    ("publish", re.compile(_START + r"(?:npm|pnpm|yarn)\s+(?:publish|release)\b")),
    ("publish", re.compile(_START + r"npm\s+run\s+release\b")),
    ("publish", re.compile(_START + r"(?:cargo\s+publish|twine\s+upload|gem\s+push)\b")),
    ("publish", re.compile(_START + r"gh\s+release\s+(?:create|upload|delete|edit)\b")),
    ("sudo", re.compile(_START + r"sudo\s")),
    ("remote_shell", re.compile(_START + r"(?:ssh|scp|sftp)\s")),
    ("remote_shell", re.compile(_START + r"rsync\s[^\n;&|]*\s[\w.@-]+:[^\s]*")),
    ("scheduler", re.compile(_START + r"crontab\s+(?!-l\b)\S")),
    ("scheduler", re.compile(_START + r"launchctl\s+(?:load|bootstrap|enable|submit)\b")),
]


_QUOTED = re.compile(r"(?<![\w-])(-c|eval)?(\s*)('[^']*'|\"(?:[^\"\\]|\\.)*\")")


def strip_heredocs(command):
    return _HEREDOC.sub("\n", command)


def strip_quoted(command):
    """Remove quoted strings (messages, titles, patterns) unless they follow -c or eval and run as code."""
    def keep_code(match):
        if match.group(1):
            return f"{match.group(1)}{match.group(2)} {match.group(3)[1:-1]} "
        return match.group(2) + "''"
    return _QUOTED.sub(keep_code, command)


def _push_targets_default_branch(segment, branch):
    words = segment.split()
    if len(words) < 2 or words[0] != "git":
        return False
    try:
        index = words.index("push")
    except ValueError:
        return False
    # Options that take a value are rare after push; treat any non-option word as remote or refspec.
    args = [word for word in words[index + 1:] if not word.startswith("-") and "<" not in word and ">" not in word]
    if any(flag in words for flag in ("--all", "--mirror")):
        return True
    refspecs = args[1:]
    for refspec in refspecs:
        destination = refspec.lstrip("+").split(":")[-1]
        if destination.removeprefix("refs/heads/") in DEFAULT_BRANCHES:
            return True
    # A push with no refspec pushes the current branch.
    return not refspecs and branch in DEFAULT_BRANCHES


def check(command, branch=None):
    """Return the names of the rules the command breaks, in a stable order without repeats."""
    text = strip_quoted(strip_heredocs(command))
    hits = [name for name, pattern in RULES if pattern.search(text)]
    if any(_push_targets_default_branch(segment.strip(), branch) for segment in _SEGMENT_SPLIT.split(text)):
        hits.append("push_default_branch")
    return list(dict.fromkeys(hits))
