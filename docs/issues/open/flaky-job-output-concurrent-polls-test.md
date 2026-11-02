# A load-dependent flake in the concurrent job_output test

`internal/tools/bashbackground_test.go`'s `TestJobOutputConcurrentPollsDoNotPanicOrDuplicate`
concatenates the content of 400 concurrent `job_output` results plus one final
`waitForBackgroundDone` result, then asserts that `line-1\n`, `line-100\n` and `line-200`
appear somewhere in that concatenation.

Each result is `Status: <state>\n\n<payload>`. A reader can catch `capturedBuffer` between
two writes of the background process, so a payload may end mid-line, and the next payload
carries the rest of that line. The concatenation then holds `line-1Status: running`, which
contains neither `line-1\n` nor a whole `line-200`. The product behaviour is right: each
call returns the bytes written since the previous read, and joining the payloads alone
reproduces the process's output exactly. The assertion is what assumes the read boundaries
fall on line boundaries.

Seen once during `go test -count=1 ./...` on 2026-11-02, with a 12848-byte combined
result. The same test passed 20 times in a row when the package ran alone, and
`go test ./internal/mcpclient ./internal/tools` passed. The machine was running every
package's tests in parallel, which is when a partial-line snapshot becomes likely.

A fix would strip each result's `Status:` header and `(no new output)` placeholder, join
the payloads, and check the joined text. That reconstructs the byte stream, so a line split
across two reads is rejoined.
