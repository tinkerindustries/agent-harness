package tools

import (
	"encoding/json"
	"strings"
)

type completeArgs struct {
	Summary string          `json:"summary"`
	Result  json.RawMessage `json:"result"`
	Status  string          `json:"status"`
}

// execComplete implements Complete. Unlike the other tools it can end the
// run, so it returns the parsed payload alongside the Result rather than
// through the generic toolFunc signature. A schema failure returns errors
// through the tool result channel and does not end the run — the model
// gets to correct it (docs/TOOLS.md).
func (e *Executor) execComplete(argsRaw json.RawMessage) (Result, CompletePayload, bool) {
	var args completeArgs
	if err := json.Unmarshal(argsRaw, &args); err != nil {
		return errorResult("invalid arguments: %v", err), CompletePayload{}, false
	}
	if strings.TrimSpace(args.Summary) == "" {
		return errorResult("summary is required"), CompletePayload{}, false
	}
	if args.Status != "" && args.Status != "done" && args.Status != "gave_up" {
		return errorResult(`status must be "done" or "gave_up", got %q`, args.Status), CompletePayload{}, false
	}

	if len(e.ResultSchema) > 0 {
		if errs := ValidateAgainstSchema(e.ResultSchema, args.Result); len(errs) > 0 {
			return Result{
				Content: "result does not match the required schema:\n- " + strings.Join(errs, "\n- "),
				IsError: true,
			}, CompletePayload{}, false
		}
	}

	return Result{Content: "Run marked complete."}, CompletePayload{
		Summary: args.Summary,
		Result:  args.Result,
		Status:  args.Status,
	}, true
}
