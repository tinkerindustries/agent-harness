package deepseek

// The response half of the Responses dialect: the `response` object a
// non-streaming call returns and a terminal stream event carries, and the
// semantic SSE frames in between
// (third_party/deepseek-docs/api/create-response.md).
//
// These are decode-only. Nothing here is ever marshalled back onto the
// wire, so field order carries none of the byte-stability weight that
// responses.go's request types do; what matters is that every field the
// loop needs is named, and that an unnamed one is ignored rather than
// failing the decode.

// Statuses a response object reports.
const (
	statusInProgress = "in_progress"
	statusCompleted  = "completed"
	statusIncomplete = "incomplete"
	statusFailed     = "failed"
)

// Semantic stream event types. The harness reads six of the twenty-odd the
// surface defines: the two text deltas, the item boundary that announces a
// function call, the arguments delta, and the three terminal events. The
// rest — content_part.added, output_item.done, in_progress, the web_search
// statuses — describe structure the loop reconstructs for itself or
// features it does not use, and are skipped rather than erroring, so a
// frame type added upstream cannot break a running session.
const (
	eventOutputItemAdded       = "response.output_item.added"
	eventReasoningTextDelta    = "response.reasoning_text.delta"
	eventOutputTextDelta       = "response.output_text.delta"
	eventFunctionCallArgsDelta = "response.function_call_arguments.delta"
	eventResponseCompleted     = "response.completed"
	eventResponseIncomplete    = "response.incomplete"
	eventResponseFailed        = "response.failed"
)

// streamEvent is one decoded `data:` payload. Every semantic event carries
// `type`; the rest of the fields are per-type and absent otherwise.
type streamEvent struct {
	Type string `json:"type"`
	// OutputIndex is the position of the output item this event belongs to.
	// It is the key the tool-call assembler uses: the function_call item's
	// index is what ties its arguments deltas to the call announced by an
	// earlier output_item.added (wire.ToolCallAssembler).
	OutputIndex int `json:"output_index"`
	// Delta is the incremental text of a *.delta event.
	Delta string `json:"delta"`
	// Item is the output item an output_item.added / .done event describes.
	Item *outputItem `json:"item"`
	// Response is the whole response object, present on created and on each
	// of the three terminal events.
	Response *responseObject `json:"response"`
}

// responseObject is the `response` resource.
type responseObject struct {
	ID                string             `json:"id"`
	Object            string             `json:"object"`
	CreatedAt         int64              `json:"created_at"`
	Status            string             `json:"status"`
	Error             *responseError     `json:"error"`
	IncompleteDetails *incompleteDetails `json:"incomplete_details"`
	Model             string             `json:"model"`
	Output            []outputItem       `json:"output"`
	Usage             *responsesUsage    `json:"usage"`
}

// responseError is the error a failed response carries.
type responseError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// incompleteDetails says why a response stopped short.
type incompleteDetails struct {
	Reason string `json:"reason"`
}

// outputItem is one item of a response's output: a reasoning block, the
// assistant message, or a function call.
type outputItem struct {
	Type      string       `json:"type"`
	ID        string       `json:"id"`
	Status    string       `json:"status"`
	Role      string       `json:"role"`
	Content   []outputPart `json:"content"`
	CallID    string       `json:"call_id"`
	Name      string       `json:"name"`
	Arguments string       `json:"arguments"`
}

// outputPart is one content part of an output item — output_text on a
// message, reasoning_text on a reasoning item.
type outputPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// responsesUsage is this surface's token accounting. The cache hit is
// nested under input_tokens_details rather than reported as a sibling total
// the way Chat Completions does it; usageFromResponses derives the miss.
type responsesUsage struct {
	InputTokens        int `json:"input_tokens"`
	InputTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokens        int `json:"output_tokens"`
	OutputTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
	TotalTokens int `json:"total_tokens"`
}
