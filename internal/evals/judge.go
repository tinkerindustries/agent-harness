package evals

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mrgeoffrich/deepseek-harness/internal/store"
	"github.com/mrgeoffrich/deepseek-harness/internal/wire"
)

// Client is the narrow seam between the judge and a model provider's API
// client, declared here where it is consumed the way internal/session
// declares its own (docs/KIMI-INTEGRATION.md §4.1, internal/CLAUDE.md). The
// judge names only what it calls — one non-streaming completion — so this is
// that one method, not the wider session.Client. *deepseek.Client and
// *kimi.Client both implement it, and cmd/harness resolves which one a judge
// model gets through the model→provider table (internal/provider,
// docs/KIMI-INTEGRATION.md §4.3).
type Client interface {
	CreateChatCompletion(ctx context.Context, intent wire.ChatIntent) (*wire.ChatCompletionResponse, error)
}

// The judge scores what the mechanical metrics cannot: whether the run
// actually did the work. It is a second model reading a rendered transcript,
// so it is a stochastic scorer and its own numbers carry noise the counters do
// not — treat a judge difference smaller than its spread as no difference, and
// read the counters first. It is blind to which variant produced a transcript,
// which is the one bias worth engineering out.

// A Verdict is the judge's read on one run.
type Verdict struct {
	// Score is 1 to 5 against the suite's rubric.
	Score int `json:"score"`
	// Completed says whether the task was finished, which a score alone
	// blurs: a thorough run that stopped short and a sloppy one that
	// finished can land on the same number.
	Completed bool   `json:"completed"`
	Reasoning string `json:"reasoning"`
}

// judgeSystemPrompt is fixed so every transcript is scored the same way, and
// says nothing about prompt variants: the judge must not be able to tell which
// arm it is reading.
const judgeSystemPrompt = `You are scoring one transcript from a coding agent, against a rubric.

Score 1 to 5:
1 - did not attempt the task, or made it worse
2 - attempted it and largely failed
3 - partially did it, with real gaps
4 - did it, with minor gaps
5 - did it fully and cleanly

Judge only what the transcript shows. Do not reward length, confidence, or a
summary that claims more than the tool calls support. A run that says it
verified something without a tool call showing the verification did not verify
it.

Reply with a JSON object and nothing else:
{"score": <1-5>, "completed": <true|false>, "reasoning": "<one or two sentences>"}`

// JudgeMaxTokens is the documented maximum output for either model
// (third_party/deepseek-docs/quick_start/pricing.md), so the judge is bounded
// by the model rather than by us. The verdict is short but the reasoning that
// reaches it is not, and a budget that runs out mid-thought returns empty
// content rather than a worse verdict. It is a ceiling, not a reservation: a
// request is billed for what it generates.
//
// Every request sends max_tokens explicitly (internal/wire/types_test.go),
// so this is a number rather than an omission.
const JudgeMaxTokens = 384 * 1024

// Judge scores transcripts with a model.
type Judge struct {
	Client Client
	Model  string
	// MaxTokens bounds the judge's reply. Zero is JudgeMaxTokens.
	MaxTokens int
}

// Score renders a session's transcript and asks the judge to grade it. An
// error here fails only this verdict: a run still keeps its mechanical
// metrics, which are the ones that do not need a network call.
func (j Judge) Score(ctx context.Context, rubric string, events []store.Event) (*Verdict, error) {
	transcript := RenderTranscript(events)
	if strings.TrimSpace(transcript) == "" {
		return nil, fmt.Errorf("evals: nothing to judge: the session recorded no tool calls or replies")
	}

	maxTokens := j.MaxTokens
	if maxTokens <= 0 {
		maxTokens = JudgeMaxTokens
	}
	resp, err := j.Client.CreateChatCompletion(ctx, wire.ChatIntent{
		Model: j.Model,
		// Thinking mode is on. Scoring a transcript against a rubric is a
		// judgement, and the reasoning is where it is made; the earlier
		// failure was a 1024-token budget that reasoning exhausted before any
		// content, not thinking itself.
		Thinking:  true,
		MaxTokens: maxTokens,
		Messages: []wire.Message{
			wire.SystemMessage(judgeSystemPrompt),
			wire.UserMessage("Rubric:\n" + rubric + "\n\nTranscript:\n" + transcript),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("evals: judge: %w", err)
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("evals: judge returned no choices")
	}

	var v Verdict
	raw := extractJSONObject(resp.Choices[0].Message.Content.String())
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil, fmt.Errorf("evals: judge reply is not the expected JSON object: %w (got %q)", err, truncate(resp.Choices[0].Message.Content.String(), 200))
	}
	if v.Score < 1 || v.Score > 5 {
		return nil, fmt.Errorf("evals: judge returned score %d, want 1 to 5", v.Score)
	}
	return &v, nil
}

// transcriptToolResultCap bounds one tool result in the rendered transcript.
// A full build log would crowd out the rest of the run and tell the judge
// nothing the first lines do not.
const transcriptToolResultCap = 600

// RenderTranscript turns a session's events into the plain text the judge
// reads: what the model said, what it called, and what came back.
func RenderTranscript(events []store.Event) string {
	var b strings.Builder
	for _, e := range events {
		switch e.Kind {
		case store.KindContentDelta:
			var p struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(e.Payload, &p) == nil {
				b.WriteString(p.Text)
			}
		case store.KindToolCall:
			var p store.ToolCallPayload
			if json.Unmarshal(e.Payload, &p) != nil {
				continue
			}
			fmt.Fprintf(&b, "\n\n[tool call] %s %s\n", p.Name, truncate(collapse(p.Arguments), 400))
		case store.KindToolResult:
			var p store.ToolResultPayload
			if json.Unmarshal(e.Payload, &p) != nil {
				continue
			}
			status := "ok"
			if p.IsError {
				status = "error"
			}
			fmt.Fprintf(&b, "[tool result] %s (%s) %s\n", p.Name, status, truncate(collapse(p.Content), transcriptToolResultCap))
		}
	}
	return strings.TrimSpace(b.String())
}

// extractJSONObject pulls the first {...} out of a reply, so a judge that
// wraps its answer in a code fence or a sentence still parses.
func extractJSONObject(s string) string {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return s
	}
	return s[start : end+1]
}

func collapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
