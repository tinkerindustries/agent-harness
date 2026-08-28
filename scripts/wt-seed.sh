#!/bin/sh
# The seed hook: copy an operator's credentials into this worktree's stack.
#
# A worktree's SQLite store lives in its own compose volume, so a fresh
# worktree starts with no DeepSeek API key and no GitHub App private key — it
# can neither call a model nor clone anything private. Typing the same two
# secrets into every new worktree is the kind of friction that gets worked
# around badly, so this copies them from a stack that already has them.
#
# Only the credentials travel: settings.SeedableCredentialKeys, minus
# http.control_token, which is generated per installation and guards that
# installation's own run-control endpoints. Nothing else in the store moves.
# A worktree that inherited work_queue rows would claim and run work queued
# for another stack, against a workspace root it does not own.
#
# Both halves run inside a container, so this needs nothing on the host but
# docker: the source stack exports from the volume it already has mounted,
# and the target imports through its own local HTTP API — which means the
# settings registry validates every write and internal/githubauth re-syncs,
# so a seeded App key is live without a restart.
#
# It runs after `start` because the target's volume does not exist until
# compose has run once.
#
# A failure here does not fail the worktree. The stack is up and works; it
# just needs its credentials set from its own settings screen.
set -eu

cd "$(dirname "$0")/.."

# The target: this worktree's own compose project, from the .env wt emitted.
project="${COMPOSE_PROJECT_NAME:-}"
if [ -z "$project" ] && [ -f .env ]; then
  project="$(sed -n 's/^COMPOSE_PROJECT_NAME=//p' .env | tail -1)"
fi
if [ -z "$project" ]; then
  echo "wt-seed: no COMPOSE_PROJECT_NAME in the environment or .env — has wt init run?" >&2
  exit 1
fi

# The source: the main checkout's own stack. Compose names a project after the
# directory it runs in when nothing overrides it, and the main checkout runs
# no `wt init`, so nothing overrides it there. Override with WT_SEED_FROM when
# the credentials live somewhere else (the production stack, say).
if [ -n "${WT_SEED_FROM:-}" ]; then
  source_container="$WT_SEED_FROM"
else
  main_root="$(dirname "$(cd "$(git rev-parse --git-common-dir)" && pwd)")"
  source_container="$(basename "$main_root" | tr '[:upper:]' '[:lower:]')-harness-1"
fi

if ! docker inspect "$source_container" >/dev/null 2>&1; then
  echo "wt-seed: the source container $source_container is not running." >&2
  echo "wt-seed: start the main checkout's stack, or set WT_SEED_FROM to another one," >&2
  echo "wt-seed: then re-run: bash scripts/wt-seed.sh" >&2
  exit 1
fi

# Export from the source, import into the target, in one pipe. Values never
# touch the host's filesystem and are never printed by either side.
docker exec "$source_container" harness seed-credentials -export \
  | docker compose -p "$project" exec -T harness harness seed-credentials -import

