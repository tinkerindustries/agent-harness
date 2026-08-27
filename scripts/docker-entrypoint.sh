#!/bin/sh
# Container entrypoint: hands off to the harness binary. The GitHub token
# is the github.token setting; harness serve turns it into a git credential
# for every clone and every git/gh call a session makes (internal/githubauth),
# so nothing about it needs setting up here.
set -e

# docker-compose.yml loads .env wholesale, which brings this stack's own
# compose controls in with it: the COMPOSE_PROJECT_NAME and port allocations
# `harness worktree init` writes for a worktree. Compose prefers the
# environment to a .env file, so a session that ran `init -standalone` in its
# own clone found its correct .env shadowed by ours, and its `docker compose
# up` recreated *our* containers instead of its own — the harness running the
# session, restarted by the session. Blanking them in the compose file is not
# enough: a variable that is set but empty still shadows .env, it just falls
# back to the compose file's directory name instead. Drop them entirely, here,
# before the harness process exists — every session runs as its child and
# inherits this environment.
#
# HARNESS_REGISTRY_DIR is deliberately not in this list: a nested compose
# needs it to find the host's worktree registry.
unset COMPOSE_PROJECT_NAME HARNESS_WORKSPACES HARNESS_HTTP_PORT HARNESS_VITE_PORT

exec harness "$@"
