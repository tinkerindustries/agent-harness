#!/bin/sh
# Container entrypoint: makes GITHUB_TOKEN usable by git before handing off
# to the harness binary. The token authenticates the clones a work request
# asks for and the git and gh calls an agent session makes afterwards.
#
# The credential file only ever exists inside the container. Configuring the
# host's global git config from a process is not the same thing.
set -e

if [ -n "${GITHUB_TOKEN}" ]; then
    umask 077
    printf 'https://x-access-token:%s@github.com\n' "${GITHUB_TOKEN}" > /root/.git-credentials
    git config --global credential.helper store
    # gh reads GH_TOKEN first and GITHUB_TOKEN second, so it needs nothing
    # here; git does not read either.
fi

exec harness "$@"
