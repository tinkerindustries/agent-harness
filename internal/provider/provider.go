// Package provider holds the single table that maps a model name to the
// provider serving it (docs/KIMI-INTEGRATION.md §4.3). Both client
// construction (cmd/harness) and request validation (internal/queue) reach
// it without importing the agent loop, which is why it is a package of its
// own rather than a helper inside internal/session or internal/deepseek.
// DeepSeek is the default provider; Kimi K3 is the second entry
// (docs/KIMI-INTEGRATION.md §5).
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
)

// models is the one model→provider table. The names are exactly what
// model.default and model.flash resolve to. The table is consulted by name,
// never by string prefix, so a model absent here fails loudly at validation
// instead of silently defaulting to a provider (docs/KIMI-INTEGRATION.md §4.3).
var models = map[string]Name{
	"deepseek-v4-pro":   DeepSeek,
	"deepseek-v4-flash": DeepSeek,
	"kimi-k3":           Kimi,
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
