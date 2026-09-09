package cache

import "github.com/mrgeoffrich/agent-harness/internal/wire"

// Mutate returns a copy of items with a marker appended to the content of
// index i, so the returned slice no longer shares a prefix with whatever was
// sent before it. Nothing in the production request path calls this; it
// exists so the churn diagnostic's catastrophic-head claim (docs/CACHE.md,
// "cheap tail, catastrophic head") can be exercised on purpose, gated behind
// an explicit debug option rather than reachable from a work request.
// items is never modified in place.
func Mutate(items []wire.Item, i int) []wire.Item {
	out := append([]wire.Item(nil), items...)
	if i < 0 || i >= len(out) {
		return out
	}
	m := out[i]
	m.Content = wire.TextItemContent(m.Content.String() + "\n\n[deliberately churned for docs/CACHE.md's diagnostic demonstration]")
	out[i] = m
	return out
}
