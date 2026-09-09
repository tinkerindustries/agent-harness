// Package responsesstdio hosts one coding session for a parent process over a
// pipe, speaking the OpenAI Responses API's vocabulary rather than a protocol
// of this harness's own.
//
// The parent — Turret, an Electron app that hosts coding sessions — spawns
// `harness stdio-session`, owns the working directory, and drives the
// session over stdin and stdout. What travels between them is JSON-RPC 2.0
// in newline-delimited JSON, and every payload inside that envelope is a
// Responses API shape: the create-response request body, the output item
// union, and the semantic event union. A client that can already read a
// `POST /responses` event stream can read this one, because the frames are
// the same frames.
//
// The provider underneath speaks the same surface: internal/deepseek posts
// to DeepSeek's own `POST /responses` (docs/DEEPSEEK-RESPONSES.md), so one
// vocabulary runs the length of the process and the item a parent reads here
// is the item the provider was sent.
//
// The agent loop underneath is internal/session, unchanged and unforked. This
// package is a translator on both sides of it: a create-response body becomes
// session.RunOptions, and the session's committed event log becomes semantic
// response events. docs/STDIO-PROTOCOL.md is the wire reference and records
// every place this deviates from what the HTTP surface does, with the reason.
package responsesstdio
