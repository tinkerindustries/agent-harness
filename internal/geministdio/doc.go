// Package geministdio hosts one coding session for a parent process over a
// pipe, speaking Google's Interactions vocabulary rather than a protocol of
// this harness's own.
//
// The parent — Turret, an Electron app that hosts coding sessions — spawns
// `harness gemini-session`, owns the working directory, and drives the
// session over stdin and stdout. What travels between them is JSON-RPC 2.0
// in newline-delimited JSON, and every payload inside that envelope is a
// shape from <https://ai.google.dev/api/interactions>: the create-interaction
// request body, the Step union, and the InteractionSseEvent union. A client
// that can already read Google's `POST /v1beta/interactions` event stream can
// read this one, because the frames are the same frames.
//
// The agent loop underneath is internal/session, unchanged and unforked. This
// package is a translator on both sides of it: a Google create-interaction
// body becomes session.RunOptions, and the session's committed event log
// becomes Google step events. docs/STDIO-PROTOCOL.md is the wire reference and
// records every place this deviates from what Google's HTTP surface does, with
// the reason.
package geministdio
