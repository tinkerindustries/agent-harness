package wire

// ChatIntent is everything the agent loop wants from one request, expressed
// provider-neutrally: the model, the conversation as items, the reasoning
// effort, whether to think, the token ceiling, and the tools. It is the seam between
// internal/session and a provider's client (docs/KIMI-INTEGRATION.md §4.1):
// the loop states intent, and each provider implementation turns it into its
// own request shape — DeepSeek's `thinking: {type}` plus `reasoning_effort`,
// Kimi K3's top-level `reasoning_effort` — so the loop never knows which
// spelling it is talking to. No provider-specific field appears here, and
// nothing here is serialised: ChatIntent is a request specification, not a
// wire body. It is declared in this package rather than in internal/session
// because the implementations live in the provider packages, which must not
// import the agent loop.
type ChatIntent struct {
	Model     string
	Items     []Item
	Effort    string
	Thinking  bool
	MaxTokens int
	Tools     []Tool
}
