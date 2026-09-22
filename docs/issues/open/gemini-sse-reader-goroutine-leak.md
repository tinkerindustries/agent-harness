# Gemini's SSE line reader can leak a goroutine

`internal/gemini/stream.go`'s `pumpChatEvents` reads lines on a goroutine that sends on an
unbuffered channel. When `pumpChatEvents` returns early — an error frame, a decode error, or
the idle watchdog firing — while the body still has unread bytes, that goroutine blocks on its
next send forever.

`internal/providerhttp.PumpStreamWith` closes the same gap with a `stopped` channel the reader
selects on. `internal/anthropic/stream.go`'s `readSSE` does the same. Carrying that pattern into
`pumpChatEvents` fixes it.

Found by the claude-provider phase 2 session
(`docs/plans/reports/claude-provider/2026-09-22-phase-2-anthropic-client.md`).
