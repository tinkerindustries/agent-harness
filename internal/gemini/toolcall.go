package gemini

// RepairArguments is a no-op. The misplaced-brace repair
// internal/deepseek's own implementation performs exists for a DeepSeek
// quirk recorded in docs/OBSERVED.md; Phase 2's live spike found zero
// malformed-JSON arguments across every call measured, up to a 69KB
// arguments body, and no generation_config lever exists on this surface to
// provoke the kind of truncation that produces one (docs/OBSERVED.md,
// "IsReasoningStarved and RepairArguments — no work found to do"). That is
// an absence-of-evidence result, not a proof the failure cannot happen on
// Gemini — if a live run ever shows malformed arguments from this provider,
// this is where the repair would go, the same place internal/kimi's own
// no-op implementation names.
func (c *Client) RepairArguments(finishReason, args string) (string, bool) {
	return args, false
}
