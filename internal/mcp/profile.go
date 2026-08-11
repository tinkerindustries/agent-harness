package mcp

import "fmt"

// resolveProfile maps the launch tool's profile argument to the model and
// effort docs/MODELS.md's per-role table names for it. "pro" leaves both
// empty, deferring to the worker pool's own configured default — which,
// absent an operator override, is exactly the main-loop row of that table
// (pro, high) — so this server never hardcodes a model name the harness
// itself might no longer agree with. "flash" cannot defer the same way: the
// pool's default is the main-loop model, not the subagent one, so it names
// flashModel (the harness's own configured flash model, threaded in from
// config) and the subagent row's effort explicitly.
func resolveProfile(profile, flashModel string) (model, effort string, err error) {
	switch profile {
	case "", "pro":
		return "", "", nil
	case "flash":
		return flashModel, "max", nil
	default:
		return "", "", fmt.Errorf(`profile must be "pro" or "flash", got %q`, profile)
	}
}
