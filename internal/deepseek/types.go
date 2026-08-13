// Package deepseek is a client for DeepSeek's native Chat Completions API
// at https://api.deepseek.com. The wire vocabulary it speaks — messages,
// tools, request and response bodies, the streaming types, the SSE scanner,
// the tool-call assembler, the stream events — lives in internal/wire,
// shared with the providers added behind it. What stays here is DeepSeek's
// own behaviour: the client and its options, the auxiliary endpoint bodies
// (/models, /user/balance), the error body, the retry classification, and
// the repairs for the quirks recorded in docs/OBSERVED.md.
package deepseek

// ModelsResponse is the body of GET /models.
type ModelsResponse struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}

// Model is one entry in ModelsResponse.
type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

// BalanceResponse is the body of GET /user/balance.
type BalanceResponse struct {
	IsAvailable  bool          `json:"is_available"`
	BalanceInfos []BalanceInfo `json:"balance_infos"`
}

// BalanceInfo is one currency's balance figures.
type BalanceInfo struct {
	Currency        string `json:"currency"`
	TotalBalance    string `json:"total_balance"`
	GrantedBalance  string `json:"granted_balance"`
	ToppedUpBalance string `json:"topped_up_balance"`
}
