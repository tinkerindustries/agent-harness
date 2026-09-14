#!/bin/sh
# The build, as one command, failing at the first thing that is wrong:
# gofmt, vet, the Go suite, a CGO_ENABLED=0 build for every release
# target, then the binary.
#
# Usage: scripts/build.sh
set -eu

cd "$(dirname "$0")/.."

stage() { printf '\n=== %s\n' "$1"; }
die() { printf '\nbuild failed: %s\n' "$1" >&2; exit 1; }

stage "gofmt"
unformatted="$(gofmt -l cmd internal)"
[ -z "$unformatted" ] || die "not gofmt-clean:$(printf '\n%s' "$unformatted")"
echo "clean"

stage "go vet"
go vet ./cmd/... ./internal/...
echo "clean"

stage "tests"
scripts/test.sh

stage "cross-compile"
for target in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 android/arm64 windows/amd64; do
	CGO_ENABLED=0 GOOS="${target%/*}" GOARCH="${target#*/}" go build -o /dev/null ./cmd/harness \
		|| die "CGO_ENABLED=0 build for $target"
	echo "$target"
done

stage "go build"
go build -o bin/harness ./cmd/harness
echo "bin/harness"
