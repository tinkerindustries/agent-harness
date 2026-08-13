package main

import (
	"strings"
	"testing"
)

// TestReasoningClaimPinsTheWireClaim asserts the parenthetical both status
// lines print — `harness run`'s "model %s (effort %s, %s)" and `harness ask`'s
// "model %s (effort %s, %s)" — says what the provider was actually asked:
// DeepSeek is sent `thinking: {type: enabled|disabled}`, so the flag is the
// truth for it; Kimi K3 is sent reasoning_effort only, never a thinking
// field, and always reasons. The kimi claim must in particular never read
// as evidence the harness sent `thinking` — sending it is an API error
// (docs/KIMI-INTEGRATION.md §2, internal/kimi/intent.go).
func TestReasoningClaimPinsTheWireClaim(t *testing.T) {
	for _, thinking := range []bool{true, false} {
		if got := reasoningClaim("kimi-k3", thinking); got != "always reasons" {
			t.Errorf("reasoningClaim(kimi-k3, %v) = %q, want \"always reasons\"", thinking, got)
		}
		if got := reasoningClaim("kimi-k3", thinking); strings.Contains(got, "thinking") {
			t.Errorf("reasoningClaim(kimi-k3, %v) = %q must not claim a thinking field was sent", thinking, got)
		}
	}
	if got := reasoningClaim("deepseek-v4-pro", true); got != "thinking enabled" {
		t.Errorf("reasoningClaim(deepseek-v4-pro, true) = %q, want \"thinking enabled\"", got)
	}
	if got := reasoningClaim("deepseek-v4-pro", false); got != "thinking disabled" {
		t.Errorf("reasoningClaim(deepseek-v4-pro, false) = %q, want \"thinking disabled\"", got)
	}
	if got := reasoningClaim("deepseek-v4-flash", true); got != "thinking enabled" {
		t.Errorf("reasoningClaim(deepseek-v4-flash, true) = %q, want \"thinking enabled\"", got)
	}
}
