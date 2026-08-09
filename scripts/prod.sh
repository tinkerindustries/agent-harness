#!/bin/sh
# Drives the production stack in docker-compose.prod.yml. Every command here
# is a plain `docker compose -f docker-compose.prod.yml ...` underneath; the
# script exists so the -f is never forgotten, because forgetting it aims the
# command at the dev stack instead.
#
# Production runs deepseek-harness:prod and never builds. `promote` is the
# only command that changes what is deployed, so a dev rebuild cannot reach
# this stack.
#
# Usage:
#   scripts/prod.sh promote [-f]   build the current checkout and move the
#                                  prod tag onto it; -f allows a dirty tree
#   scripts/prod.sh deploy         restart the stack onto the current prod tag
#   scripts/prod.sh up|down        start / stop
#   scripts/prod.sh logs [svc]     follow logs, harness by default
#   scripts/prod.sh status         containers, deployed image, queue health
#   scripts/prod.sh rollback SHA   move the prod tag back to a prod-SHA image
#   scripts/prod.sh <anything>     passed through to docker compose
set -eu

cd "$(dirname "$0")/.."

compose() {
	docker compose -f docker-compose.prod.yml "$@"
}

# The tag the compose file runs, and the immutable per-commit tag beside it
# that rollback selects from.
IMAGE=deepseek-harness:prod

usage() {
	sed -n '2,24p' "$0" | sed 's/^# \{0,1\}//'
}

cmd=${1:-status}
[ $# -gt 0 ] && shift

case "$cmd" in
promote)
	force=no
	[ "${1:-}" = "-f" ] && force=yes

	sha=$(git rev-parse --short HEAD)
	if [ "$force" = no ] && [ -n "$(git status --porcelain)" ]; then
		echo "prod.sh: the tree is dirty; promoting would deploy uncommitted work." >&2
		echo "prod.sh: commit it, or re-run with -f to promote anyway." >&2
		exit 1
	fi
	[ "$force" = yes ] && sha="$sha-dirty"

	# Built straight from the Dockerfile rather than through compose, so the
	# result does not depend on the dev project's image names or build state.
	# The per-commit tag is what rollback later selects; the moving prod tag
	# is what the compose file runs.
	docker build -t "deepseek-harness:prod-$sha" .
	docker tag "deepseek-harness:prod-$sha" "$IMAGE"

	echo
	echo "prod.sh: promoted $sha. Nothing is deployed until:"
	echo "         scripts/prod.sh deploy"
	;;

deploy)
	# up -d alone will not replace a running container whose service
	# definition is unchanged, and only the image behind the tag has moved.
	compose up -d --force-recreate --wait "$@"
	;;

up)
	compose up -d --wait "$@"
	;;

down)
	compose down "$@"
	;;

logs)
	compose logs -f "${1:-harness}"
	;;

status)
	compose ps
	echo
	echo "Deployed image:"
	# LastTagTime, not Created: a rebuild off cached layers inherits the
	# original config timestamp, so Created reports the first promote no
	# matter how many have happened since. LastTagTime is when this tag was
	# moved here, which is what "when was this promoted" means.
	docker image inspect "$IMAGE" --format \
		'  {{.Id}}
  promoted {{.Metadata.LastTagTime}}
  tags     {{join .RepoTags ", "}}' 2>/dev/null ||
		echo "  $IMAGE does not exist yet — run scripts/prod.sh promote"
	echo
	echo "Health:"
	echo "  harness  $(curl -sf --max-time 3 localhost:8180/api/queue || echo unreachable)"
	# A bare GET of /mcp is a 405 — the endpoint takes POST. Any status code
	# at all means the server answered, so -f would report a live server as
	# unreachable.
	echo "  mcp      HTTP $(curl -so /dev/null --max-time 3 -w '%{http_code}' localhost:8190/mcp || echo unreachable)"
	;;

rollback)
	sha=${1:-}
	if [ -z "$sha" ]; then
		echo "prod.sh: rollback needs a commit. Available:" >&2
		docker images 'deepseek-harness:prod-*' \
			--format '  {{.Tag}}  {{.CreatedSince}}' >&2
		exit 1
	fi
	docker tag "deepseek-harness:prod-$sha" "$IMAGE"
	echo "prod.sh: prod tag moved to prod-$sha. Deploy it with:"
	echo "         scripts/prod.sh deploy"
	;;

help | -h | --help)
	usage
	;;

*)
	compose "$cmd" "$@"
	;;
esac
