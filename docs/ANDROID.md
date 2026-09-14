# Running on Android

The harness builds as a native Android binary with
`CGO_ENABLED=0 GOOS=android GOARCH=arm64 go build ./cmd/harness`, and runs
under Termux. `scripts/build.sh` cross-compiles that target on every build.

## What the process needs from its host

- **A writable state directory.** The default lives under `$HOME/.cache`, and
  falls back to `$TMPDIR` when `$HOME` is unset. Without either, Go picks
  `/data/local/tmp`, which an app's own uid cannot write. Pass `-state-dir`,
  or spawn the process with Termux's `HOME` and `TMPDIR`.
- **bash on `PATH`.** The `Bash` tool runs through bash when `PATH` has it and
  through `/system/bin/sh` when not. Termux's `bash` package supplies bash.
- **ripgrep**, from Termux's `ripgrep` package, named with `-rg` or found on
  `PATH`. Without it `Grep` uses its own walk.
- **`-prices`** as an absolute path, since the default is relative to the
  working directory.

## DNS

A `CGO_ENABLED=0` binary resolves names with Go's own resolver, which reads
`/etc/resolv.conf`. Android has no such file, and Go then sends queries to
`127.0.0.1:53` and `[::1]:53`, where nothing listens. `internal/androiddns`
redirects those two addresses when the process runs on Android without
`/etc/resolv.conf`. It uses the `nameserver` lines of Termux's
`$PREFIX/etc/resolv.conf`, and `8.8.8.8` and `1.1.1.1` when that file has
none. On a network that blocks public DNS, put a reachable server in that file.
The process logs the servers it chose to stderr at startup.

## CA roots

Go reads Android's system roots from `/system/etc/security/cacerts`. No
harness code is involved. `SSL_CERT_FILE` and `SSL_CERT_DIR` override them in
the usual way.

## Checking a build on a device or emulator

Push the binary and `configs/prices.json` to `/data/local/tmp`, set `HOME` and
`TMPDIR` to a writable directory there, and pipe `initialize`, `initialized`
and a `responses.create` into `harness stdio-session` with a placeholder
`DEEPSEEK_API_KEY`. A working build reaches DeepSeek and fails the run with
`401 authentication_error`. A broken resolver fails it with
`lookup api.deepseek.com on [::1]:53`.
