#!/bin/sh
# Container entrypoint: hands off to the harness binary. The GitHub
# credential is a setting — github.token, or the GitHub App's github.app_id
# and github.app_private_key; harness serve turns whichever is set into a git
# credential for every clone and every git/gh call a session makes
# (internal/githubauth, docs/GITHUB-APP.md), so nothing about it needs setting
# up here.
set -e

# docker-compose.yml loads .env wholesale, which brings this stack's own
# compose controls in with it: the COMPOSE_PROJECT_NAME and port allocations
# `wt init` writes for a worktree. Compose prefers the environment to a .env
# file, so a session that wrote its own .env in its own clone found it shadowed
# by ours, and its `docker compose up` recreated *our* containers instead of
# its own — the harness running the session, restarted by the session.
# Blanking them in the compose file is not enough: a variable that is set but
# empty still shadows .env, it just falls back to the compose file's directory
# name instead. Drop them entirely, here, before the harness process exists —
# every session runs as its child and inherits this environment.
#
# Unsetting is necessary but no longer sufficient. With them gone, compose in a
# session's clone falls back to the clone's own directory basename — which for
# a clone of this repo is the *host dev stack's* project name. A session that
# brings a stack up in its own clone must set COMPOSE_PROJECT_NAME and its
# ports itself; assets/skills/deepseek-flash-task/SKILL.md says so, and
# docs/wt.md's "Agent sessions inside the harness container" explains why
# nothing does it for them any more.
unset COMPOSE_PROJECT_NAME HARNESS_WORKSPACES HARNESS_HTTP_PORT HARNESS_VITE_PORT

exec harness "$@"
