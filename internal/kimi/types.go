// Package kimi is a client for Moonshot AI's Kimi API at
// https://api.moonshot.ai/v1 (third_party/kimi-docs/api/overview.md). The
// wire vocabulary it speaks — messages, tools, request and response bodies,
// the streaming types, the SSE scanner, the tool-call assembler, the stream
// events — lives in internal/wire, shared with the DeepSeek client. What
// stays here is Kimi's own behaviour: the client and its options, the
// auxiliary endpoint bodies (/models, /users/me/balance), the error body,
// the retry classification, and the usage split that maps Kimi's single
// cached_tokens figure onto the cache-hit/cache-miss counts the cost model
// uses. It implements the narrow Client seam internal/session declares
// (docs/KIMI-INTEGRATION.md §4.1): it turns the loop's wire.ChatIntent into
// Kimi's request shape — top-level reasoning_effort, never a thinking field —
// and knows nothing of sessions, tools, or storage. Depends on:
// internal/wire.
package kimi

// ModelsResponse is the body of GET /models: an OpenAI-format object/list of
// model objects, each carrying the capability flags the docs list
// (third_party/kimi-docs/api/list-models.md).
type ModelsResponse struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}

// Model is one entry in ModelsResponse.
type Model struct {
	ID                string `json:"id"`
	Object            string `json:"object"`
	Created           int64  `json:"created"`
	OwnedBy           string `json:"owned_by"`
	ContextLength     int    `json:"context_length"`
	SupportsImageIn   bool   `json:"supports_image_in"`
	SupportsVideoIn   bool   `json:"supports_video_in"`
	SupportsReasoning bool   `json:"supports_reasoning"`
}

// BalanceResponse is the body of GET /users/me/balance: a code/data/scode/
// status envelope whose data carries the USD balances
// (third_party/kimi-docs/api/balance.md, openapi.json BalanceResponse).
type BalanceResponse struct {
	Code   int         `json:"code"`
	Data   BalanceData `json:"data"`
	Scode  string      `json:"scode"`
	Status bool        `json:"status"`
}

// BalanceData is one account's USD balances. AvailableBalance is the sum
// that gates API calls: when it is ≤ 0 the API refuses requests with
// exceeded_current_quota_error (third_party/kimi-docs/api/balance.md).
type BalanceData struct {
	AvailableBalance float64 `json:"available_balance"`
	VoucherBalance   float64 `json:"voucher_balance"`
	CashBalance      float64 `json:"cash_balance"`
}
