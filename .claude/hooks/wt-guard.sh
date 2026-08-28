#!/bin/sh
# PreToolUse enforcement hook, generated per repo by worktree-manager's
# onboarding skill. It denies a tool call whose file_path leaves the worktree
# the session is standing in, so an unattended run working in one worktree
# cannot write into another one — or into the main checkout.
#
# This repo dispatches agent runs that nobody is watching (assets/skills/
# deepseek-flash-task and deepseek-flash-autopilot), which is the case the
# guard exists for. Committing it makes the enforcement choice for everyone
# who clones this repo; that was a deliberate call, recorded in
# docs/wt-decision-record.md.
#
# The engine is `wt guard`, which is local and opens no socket: it classifies
# cwd, contains the file_path, and denies with a reason naming the correct
# root verbatim. This script only maps its answer onto the hook protocol.
#
# It fails OPEN. A missing `wt`, an unreadable descriptor or any exit code the
# contract does not define lets the call through and says so on stderr —
# a broken guard must not make the repository unworkable.
set -u

# The per-session classification cache. `wt guard` reads the descriptor fresh
# on every call and caches only the classification here, so a worktree removed
# mid-session drops the cache, the next call reclassifies, and the guard fails
# open with a one-time note rather than denying against a tree that is gone.
WT_GUARD_CACHE="${WT_GUARD_CACHE:-${TMPDIR:-/tmp}/wt-guard-${CLAUDE_SESSION_ID:-$PPID}}"
export WT_GUARD_CACHE

command -v wt >/dev/null 2>&1 || {
  echo "wt-guard: wt is not on PATH; allowing this call unguarded" >&2
  exit 0
}

payload="$(cat)"
answer="$(printf '%s' "$payload" | wt guard --json 2>/dev/null)"
status=$?

case "$status" in
  0)
    # Allowed. Say nothing: the hook is silent on the happy path.
    exit 0
    ;;
  3)
    # Denied. Exit 2 is the PreToolUse protocol's "block", and stderr is what
    # the model is shown — so hand it wt's own reason, which names the root
    # the call should have used.
    reason="$(printf '%s' "$answer" | sed -n 's/.*"reason"[[:space:]]*:[[:space:]]*"\(.*\)".*/\1/p' | head -n1)"
    [ -n "$reason" ] || reason="this path is outside the worktree this session is working in"
    echo "wt-guard: $reason" >&2
    exit 2
    ;;
  *)
    echo "wt-guard: wt guard exited $status; allowing this call unguarded" >&2
    exit 0
    ;;
esac

# --- managed by wt; edits below are overwritten ---
# wt-field: app=agent-harness
# wt-field: band harness_http=8700
# wt-field: band vite=5700
# wt-field: descriptor=wt-env.json
# wt-field: resources=harness_http, vite, compose, workspaces
# wt-field: shared=the host docker socket, the deepseek-harness-prod stack, harness-unity-state (docker volume), harness-unity:latest (docker image), the DeepSeek account and the GitHub App installation, the Go module cache and the npm cache
# wt-field: worktrees=.claude/worktrees/{slug}
# --- end ---
