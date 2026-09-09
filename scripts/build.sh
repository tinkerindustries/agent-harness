#!/bin/sh
# The build, as one command, failing at the first thing that is wrong:
# gofmt, vet, the Go suite, then the binary.
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

stage "go build"
go build -o bin/harness ./cmd/harness
echo "bin/harness"
