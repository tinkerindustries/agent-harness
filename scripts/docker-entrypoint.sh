#!/bin/sh
# Container entrypoint: makes GITHUB_TOKEN usable by git before handing off
# to the harness binary. The token authenticates the clones a work request
# asks for and the git and gh calls an agent session makes afterwards.
#
# The credential file only ever exists inside the container. Configuring the
# host's global git config from a process is not the same thing.
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

if [ -n "${GITHUB_TOKEN}" ]; then
    umask 077
    printf 'https://x-access-token:%s@github.com\n' "${GITHUB_TOKEN}" > /root/.git-credentials
    git config --global credential.helper store
    # gh reads GH_TOKEN first and GITHUB_TOKEN second, so it needs nothing
    # here; git does not read either.
fi

exec harness "$@"
