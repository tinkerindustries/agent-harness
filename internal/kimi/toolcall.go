package kimi

// RepairArguments is a no-op, and deliberately so: the misplaced-brace
// repair exists for a DeepSeek behaviour recorded in docs/OBSERVED.md, and
// this phase makes no live Kimi calls, so there is nothing observed to
// repair against. K3's streamed tool-call arguments assemble through the
// same wire.ToolCallAssembler as DeepSeek's, and if a live Phase 6 run ever
// shows malformed arguments from Kimi, this method — already called by the
// loop at the one place a repair could land — is where that repair would go
// (internal/session/turn.go, docs/KIMI-INTEGRATION.md §6).
func (c *Client) RepairArguments(finishReason, args string) (string, bool) {
	return args, false
}
