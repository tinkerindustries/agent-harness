// Package provider holds the single table that maps a model name to the
// provider serving it (docs/KIMI-INTEGRATION.md §4.3). Both client
// construction (cmd/harness) and request validation (internal/queue) reach
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
	// Gemini is Google's Gemini API, serving gemini-3.7-flash through the
	// Interactions surface (docs/GEMINI-INTEGRATION.md). internal/gemini also
	// serves the vision tools (Glance, Ground, Detect) on this same provider,
	// but that path is called directly (session.Runner.Gemini) rather than
	// through this table — this entry is only for the agentic coding seam.
	Gemini Name = "gemini"
)

// models is the one model→provider table. The names are exactly what
// model.default and model.flash resolve to. The table is consulted by name,
// never by string prefix, so a model absent here fails loudly at validation
// instead of silently defaulting to a provider (docs/KIMI-INTEGRATION.md §4.3).
var models = map[string]Name{
	"deepseek-v4-pro":              DeepSeek,
	"deepseek-v4-flash":            DeepSeek,
	"deepseek-v4-flash-vision-exp": DeepSeek,
	"kimi-k3":                      Kimi,
	"gemini-3.7-flash":             Gemini,
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
// images natively. It answers per model rather than per provider because
// deepseek-v4-flash-vision-exp is the first model whose vision capability
// disagrees with its provider's: DeepSeek's other two models don't see
// images, but this one does (docs/DEEPSEEK-VISION.md). Kimi K3
// (docs/KIMI-INTEGRATION.md §4.5) and Gemini (docs/GEMINI-INTEGRATION.md
// §5.7) both see images too, so DeepSeek is now the only provider whose
// models disagree with each other, which is exactly why this table is keyed
// by model rather than by provider. A model absent from this table resolves
// to false, the same as an unknown model resolves to false everywhere else
// in this package, rather than panicking or erroring — callers such as
// internal/tools.DefinitionsFor already fall back to the DeepSeek-shaped
// default for a model ModelFor rejects, and SeesImages must agree with that
// fallback rather than fail a different way.
var visionCapable = map[string]bool{
	"deepseek-v4-pro":              false,
	"deepseek-v4-flash":            false,
	"deepseek-v4-flash-vision-exp": true,
	"kimi-k3":                      true,
	"gemini-3.7-flash":             true,
}

// SeesImages reports whether model reads images natively. It is the one
// source of truth both halves of the vision split consult: which tool array
// a session sends (internal/tools.DefinitionsFor) and whether Read returns
// an image part (internal/session's seesImages, tools.Executor.SeeImages).
func SeesImages(model string) bool {
	return visionCapable[model]
}
