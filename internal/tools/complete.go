package tools

import (
	"encoding/json"
	"sort"
	"strconv"
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
			msg := "result does not match the required schema:\n- " + strings.Join(errs, "\n- ")
			if hint := flatResultHint(e.ResultSchema, args.Result, argsRaw); hint != "" {
				msg += "\n\n" + hint
			}
			return Result{Content: msg, IsError: true}, CompletePayload{}, false
		}
	}

	return Result{Content: "Run marked complete."}, CompletePayload{
		Summary: args.Summary,
		Result:  args.Result,
		Status:  args.Status,
	}, true
}

// completeParams are Complete's own arguments, which are never schema fields
// however the result schema is shaped.
var completeParams = map[string]bool{"summary": true, "result": true, "status": true}

// flatResultHint names the one mistake that "result: expected object, got
// null" does not name: the schema's fields sent as top-level arguments
// beside status instead of nested inside result. The generic error is
// accurate but describes the absent argument rather than the present ones,
// and a live run spent eleven consecutive Complete calls varying the payload
// size rather than its shape before ending on no_tool_calls
// (docs/reviews/sess-bb6c0ed564ddae573c3b1832cb3981f4.md). It returns "" for
// every other failure, so a genuinely malformed result still gets only the
// validator's own errors.
func flatResultHint(schema, result, argsRaw json.RawMessage) string {
	// Only a missing result can be this mistake. A result that is present
	// but wrong is a different problem and the validator already describes
	// it well.
	if len(result) > 0 && string(result) != "null" {
		return ""
	}
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(schema, &s); err != nil || len(s.Properties) == 0 {
		return ""
	}
	var sent map[string]json.RawMessage
	if err := json.Unmarshal(argsRaw, &sent); err != nil {
		return ""
	}
	var strays []string
	for k := range sent {
		if completeParams[k] {
			continue
		}
		if _, isSchemaField := s.Properties[k]; isSchemaField {
			strays = append(strays, k)
		}
	}
	if len(strays) == 0 {
		return ""
	}
	sort.Strings(strays)
	return "result is missing, but " + quotedList(strays) + " arrived as top-level " +
		"arguments. The schema describes the value of the result argument, not Complete's " +
		"own arguments: nest those fields inside result, as " +
		`Complete(status=…, summary=…, result={"` + strays[0] + `": …}).`
}

func quotedList(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = strconv.Quote(n)
	}
	switch len(quoted) {
	case 1:
		return quoted[0]
	case 2:
		return quoted[0] + " and " + quoted[1]
	default:
		return strings.Join(quoted[:len(quoted)-1], ", ") + ", and " + quoted[len(quoted)-1]
	}
}
