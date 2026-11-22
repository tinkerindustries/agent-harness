package gemini

import "slices"

// Values for the top-level service_tier request field. Standard is what the
// API serves when the field is absent, so it is never sent.
const (
	ServiceTierStandard = "standard"
	ServiceTierFlex     = "flex"
)

// flexModels are the models Google's Flex inference page lists as supporting
// service_tier "flex" that this repository routes. docs/GEMINI-INTEGRATION.md,
// "Service tiers", records the source and what has not been confirmed live.
var flexModels = map[string]bool{
	"gemini-4-neon":         true,
	"gemini-4-hydrogen":     true,
	"gemini-4-carbon":       true,
	"gemini-4-argon":        true,
	"gemini-3.8-flash":      true,
	"gemini-3.6-flash":      true,
	"gemini-3.5-flash-lite": true,
}

// ServiceTiersFor returns the non-default service tiers model accepts, or nil
// when it accepts none.
func ServiceTiersFor(model string) []string {
	if flexModels[model] {
		return []string{ServiceTierFlex}
	}
	return nil
}

// ServiceTierSupported reports whether a create naming tier for model may
// reach the API. The standard tier, spelled or omitted, is always supported.
// A model absent from the table is refused for flex.
func ServiceTierSupported(model, tier string) bool {
	if tier == "" || tier == ServiceTierStandard {
		return true
	}
	return slices.Contains(ServiceTiersFor(model), tier)
}

// requestServiceTier is the value the request carries for tier: empty, which
// omits the field, for the standard tier.
func requestServiceTier(tier string) string {
	if tier == ServiceTierStandard {
		return ""
	}
	return tier
}
