// Package provider holds the single table that maps a model name to the
// provider serving it (docs/KIMI-INTEGRATION.md §4.3). Both client
// construction (cmd/harness) and request validation reach
// it without importing the agent loop, which is why it is a package of its
// own rather than a helper inside internal/session or internal/deepseek.
// DeepSeek is the default provider; Kimi K3 was the second entry
// (docs/KIMI-INTEGRATION.md §5), and Gemini the third
// (docs/GEMINI-INTEGRATION.md §7 Phase 5).
package provider

import (
	"fmt"
	"sort"
	"strings"
)

// Name identifies a model provider.
type Name string

const (
	// DeepSeek is the default provider. Kimi K3 joined it in Phase 5
	// (docs/KIMI-INTEGRATION.md §5).
	DeepSeek Name = "deepseek"
	// Kimi is Moonshot AI's platform, serving kimi-k3 (third_party/kimi-docs/).
	Kimi Name = "kimi"
	// Gemini is Google's Gemini API, serving gemini-3.8-flash, gemini-3.7-flash,
	// gemini-3.6-flash, gemini-3.5-flash and gemini-3.5-flash-lite through the
	// Interactions surface (docs/GEMINI-INTEGRATION.md). internal/gemini also
	// serves the vision tools (Glance, Ground, Detect) on this same provider,
	// but that path is called directly (session.Runner.Gemini) rather than
	// through this table — this entry is only for the agentic coding seam.
	Gemini Name = "gemini"
	// Anthropic is Claude's own Messages API, serving claude-opus-5-5,
	// claude-sonnet-5-5 and claude-fable-5-1 (docs/ANTHROPIC-INTEGRATION.md).
	Anthropic Name = "anthropic"
)

// models is the one model→provider table. The names are exactly what
// model.default and model.flash resolve to. The table is consulted by name,
// never by string prefix, so a model absent here fails loudly at validation
// instead of silently defaulting to a provider (docs/KIMI-INTEGRATION.md §4.3).
var models = map[string]Name{
	"deepseek-flash":        DeepSeek,
	"kimi-k3":               Kimi,
	"gemini-3.8-flash":      Gemini,
	"gemini-3.7-flash":      Gemini,
	"gemini-3.6-flash":      Gemini,
	"gemini-3.5-flash":      Gemini,
	"gemini-3.5-flash-lite": Gemini,
	"claude-opus-5-5":       Anthropic,
	"claude-sonnet-5-5":     Anthropic,
	"claude-fable-5-1":      Anthropic,
}

// ModelFor returns the provider that serves model. The table has no
// default: an unknown model is an error, so a typo surfaces at validation
// rather than routing to the wrong provider mid-run.
func ModelFor(model string) (Name, error) {
	if p, ok := models[model]; ok {
		return p, nil
	}
	return "", fmt.Errorf("provider: unknown model %q (known: %s)", model, strings.Join(KnownModels(), ", "))
}

// Known reports whether model is in the table.
func Known(model string) bool {
	_, ok := models[model]
	return ok
}

// KnownModels lists every model in the table, sorted, for error text and
// callers that enumerate the supported names.
func KnownModels() []string {
	out := make([]string, 0, len(models))
	for m := range models {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// visionCapable is the one model→capability table for whether a model reads
// images natively. It answers per model rather than per provider because a
// future DeepSeek model could disagree with deepseek-flash the way
// deepseek-v4-pro used to before this harness dropped it
// (docs/DEEPSEEK-VISION.md): keying by provider would have no way to say so.
// Kimi K3 (docs/KIMI-INTEGRATION.md §4.5) and Gemini
// (docs/GEMINI-INTEGRATION.md §5.7) both see images too. A model absent from
// this table resolves to false, the same as an unknown model resolves to
// false everywhere else in this package, rather than panicking or erroring —
// callers such as internal/tools.DefinitionsFor already fall back to the
// DeepSeek-shaped default for a model ModelFor rejects, and SeesImages must
// agree with that fallback rather than fail a different way.
//
// gemini-3.6-flash, gemini-3.5-flash and gemini-3.5-flash-lite are set true
// alongside gemini-3.7-flash: third_party/gemini-docs/models.md describes
// 3.6 Flash's "multimodal capabilities" directly, and
// third_party/gemini-docs/pricing.md prices 3.5 Flash-Lite's input
// uniformly across "text / image / video / audio" — the same undifferentiated
// per-modality pricing 3.7-flash itself carries — with nothing in the mirror
// suggesting the 3.5/3.6 generation dropped image input that the 3.7 one has.
// gemini-3.8-flash is set true on firmer ground: it postdates that mirror
// (released after 2026-08-21), but its own live page at
// ai.google.dev/gemini-api/docs/models/gemini-3.8-flash states its input
// types outright — "Text, Image, Video, Audio, and PDF" — read 2026-09-08.
// claude-opus-5-5, claude-sonnet-5-5 and claude-fable-5-1 are set true on the
// same grounds as every other entry here reading images is a trained-in
// capability of the model itself, and Anthropic's own vision docs
// (platform.claude.com/docs/en/build-with-claude/vision) describe image
// input as a feature of the Claude models generally, not a per-tool
// capability internal/anthropic adds.
var visionCapable = map[string]bool{
	"deepseek-flash":        true,
	"kimi-k3":               true,
	"gemini-3.8-flash":      true,
	"gemini-3.7-flash":      true,
	"gemini-3.6-flash":      true,
	"gemini-3.5-flash":      true,
	"gemini-3.5-flash-lite": true,
	"claude-opus-5-5":       true,
	"claude-sonnet-5-5":     true,
	"claude-fable-5-1":      true,
}

// SeesImages reports whether model reads images natively. It is the one
// source of truth both halves of the vision split consult: which tool array
// a session sends (internal/tools.DefinitionsFor) and whether Read returns
// an image part (internal/session's seesImages, tools.Executor.SeeImages).
func SeesImages(model string) bool {
	return visionCapable[model]
}
