#!/bin/sh
# Runs the Go suite. The suite needs no broker and no other service: every
# test uses a fresh SQLite store in a temp dir, so this script is the whole
# job.
#
# Usage: scripts/test.sh [go test flags...]   runs every package.
set -eu

cd "$(dirname "$0")/.."

# Every package in this module except the agent workspaces.
#
# The harness writes each session's workspace into workspaces/ (dev) and
# workspaces-prod/ (prod), both inside this checkout, so whatever Go a
# session happens to leave there is part of this module as far as the go
# tool is concerned. One prod session's scratch/ holds two files that each
# declare main, which makes `go test ./...` fail to build a package that is
# not ours, was never ours, and cannot be deleted — workspaces-prod/ belongs
# to the running production stack (see CLAUDE.md). That failure is worse
# than noise: it makes the suite exit non-zero every single run, so a real
# failure has nothing left to signal with.
#
# Filtering the listed packages rather than naming ./cmd/... ./internal/...
# is deliberate. A hardcoded pair of roots silently stops covering a new
# top-level package, and tests that quietly do not run are a worse bug than
# the one being fixed here.
packages=$(go list ./... | grep -v "/workspaces") || exit 1

go test $packages "$@"
