package anthropic

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// ErrIdleTimeout is sent when no SSE frame arrives within the idle window —
// the same watchdog internal/deepseek, internal/kimi and internal/gemini
// run on their own streams (docs/DESIGN.md §4.3), reimplemented here
// because this client's frame vocabulary is its own (named SSE events, a
// content-block-indexed delta shape) rather than either of theirs.
var ErrIdleTimeout = errors.New("anthropic: stream idle timeout")

// defaultIdleTimeout is readSSE's watchdog window when none is set.
const defaultIdleTimeout = 120 * time.Second

// maxStreamLine bounds one SSE data line. A thinking signature and a large
// tool_use input can each run to tens of kilobytes; this is headroom rather
// than a working limit.
const maxStreamLine = 1 << 20

func (c *Client) idleTimeout() time.Duration {
	if c.idleTimeoutOverride > 0 {
		return c.idleTimeoutOverride
	}
	return defaultIdleTimeout
}

// streamResult is what one HTTP response's SSE stream produced: the raw
// content blocks in order, the stop reason and its stop_details, and the
// merged usage (mergeUsage's own doc comment).
type streamResult struct {
	blocks      []json.RawMessage
	stopReason  string
	stopDetails json.RawMessage
	usage       *wire.Usage
}

// blockState accumulates one content_block's fields across its start and
// delta frames until the stream ends, when render turns it into the raw
// block bytes the replay array carries.
type blockState struct {
	typ       string
	text      strings.Builder
	thinking  strings.Builder
	signature string
	id        string
	name      string
	input     strings.Builder
	data      string
	// startRaw is the content_block object exactly as content_block_start
	// carried it, kept as a fallback for a block kind this client does not
	// reassemble from deltas — chiefly a *_tool_result block, which the
	// streaming guide's own examples show arriving whole at
	// content_block_start with no deltas at all.
	startRaw json.RawMessage
}

func (b *blockState) render() json.RawMessage {
	switch b.typ {
	case "text":
		return marshalOrEmpty(rawContentBlock{Type: "text", Text: b.text.String()})
	case "thinking":
		return marshalOrEmpty(rawContentBlock{Type: "thinking", Thinking: b.thinking.String(), Signature: b.signature})
	case "redacted_thinking":
		return marshalOrEmpty(rawContentBlock{Type: "redacted_thinking", Data: b.data})
	case "tool_use", "server_tool_use":
		input := b.input.String()
		if input == "" {
			input = "{}"
		}
		return marshalOrEmpty(rawContentBlock{Type: b.typ, ID: b.id, Name: b.name, Input: json.RawMessage(input)})
	default:
		if len(b.startRaw) > 0 {
			return b.startRaw
		}
		return marshalOrEmpty(rawContentBlock{Type: b.typ})
	}
}

func marshalOrEmpty(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

// contentBlockShape reads just enough of a content_block_start's
// content_block object to open a blockState; the object's remaining bytes
// are kept verbatim in blockState.startRaw for the kinds this client does
// not reassemble from deltas.
type contentBlockShape struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Name string `json:"name"`
}

type sseContentBlockStart struct {
	Index        int             `json:"index"`
	ContentBlock json.RawMessage `json:"content_block"`
}

type sseContentBlockDelta struct {
	Index int `json:"index"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		Signature   string `json:"signature"`
		PartialJSON string `json:"partial_json"`
	} `json:"delta"`
}

type sseMessageStart struct {
	Message struct {
		Usage *messagesUsage `json:"usage"`
	} `json:"message"`
}

type sseMessageDelta struct {
	Delta struct {
		StopReason  string          `json:"stop_reason"`
		StopDetails json.RawMessage `json:"stop_details,omitempty"`
	} `json:"delta"`
	Usage *messagesUsage `json:"usage"`
}

// mergeUsage combines message_start's usage — the only frame that reliably
// carries input_tokens and the cache figures — with message_delta's, whose
// output_tokens is the cumulative, authoritative total
// (docs/ANTHROPIC-INTEGRATION.md's own streaming example shows a
// message_delta usage carrying output_tokens alone). A delta field of zero
// is read as absent and falls back to message_start's value, since neither
// object distinguishes "zero" from "not sent" any other way on this
// surface.
func mergeUsage(start, delta *messagesUsage) *wire.Usage {
	if start == nil && delta == nil {
		return nil
	}
	var s, d messagesUsage
	if start != nil {
		s = *start
	}
	if delta != nil {
		d = *delta
	}
	pick := func(preferred, fallback int) int {
		if preferred != 0 {
			return preferred
		}
		return fallback
	}
	input := pick(d.InputTokens, s.InputTokens)
	cacheRead := pick(d.CacheReadInputTokens, s.CacheReadInputTokens)
	cacheWrite := pick(d.CacheCreationInputTokens, s.CacheCreationInputTokens)
	output := pick(d.OutputTokens, s.OutputTokens)
	return &wire.Usage{
		PromptTokens:     input + cacheRead + cacheWrite,
		CompletionTokens: output,
		TotalTokens:      input + cacheRead + cacheWrite + output,
		CachedTokens:     cacheRead,
		CacheWriteTokens: cacheWrite,
	}
}

// readSSE reads body as one Messages API SSE stream, forwarding text,
// thinking and tool-call deltas to send as they arrive and returning the
// complete raw content-block array once the stream ends at message_stop.
// It owns body and closes it. An error event, a decode error, the idle
// watchdog or ctx cancellation all end the read with an error; a decode
// error and an idle timeout leave body's underlying connection for the
// caller to have closed via the defer, matching internal/gemini's
// pumpChatEvents shape for the same surface-specific reason: no shared
// frame vocabulary with internal/providerhttp.PumpStream to reuse.
func (c *Client) readSSE(ctx context.Context, body io.ReadCloser, send func(wire.Event) bool) (streamResult, error) {
	defer body.Close()

	// stopped releases the line-reading goroutine below when readSSE
	// returns for any reason other than the line reader closing lines
	// itself — an error event, a decode error, the idle watchdog. Without
	// it that goroutine can block forever on an unbuffered send nobody is
	// left to receive, holding the response body's connection open with it
	// (providerhttp.PumpStreamWith's own "stopped" channel exists for the
	// identical reason).
	stopped := make(chan struct{})
	defer close(stopped)

	lines := make(chan string)
	lineErrs := make(chan error, 1)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(body)
		sc.Buffer(make([]byte, 0, 64*1024), maxStreamLine)
		for sc.Scan() {
			select {
			case lines <- sc.Text():
			case <-ctx.Done():
				return
			case <-stopped:
				return
			}
		}
		if err := sc.Err(); err != nil {
			select {
			case lineErrs <- err:
			case <-stopped:
			}
		}
	}()

	idle := time.NewTimer(c.idleTimeout())
	defer idle.Stop()

	blocks := map[int]*blockState{}
	var order []int
	var startUsage, deltaUsage *messagesUsage
	var stopReason string
	var stopDetails json.RawMessage

	var event, data string
	flush := func() (done bool, err error) {
		if event == "" && data == "" {
			return false, nil
		}
		e, d := event, data
		event, data = "", ""
		switch e {
		case "message_start":
			var f sseMessageStart
			if err := json.Unmarshal([]byte(d), &f); err != nil {
				return false, err
			}
			startUsage = f.Message.Usage

		case "content_block_start":
			var f sseContentBlockStart
			if err := json.Unmarshal([]byte(d), &f); err != nil {
				return false, err
			}
			var shape contentBlockShape
			json.Unmarshal(f.ContentBlock, &shape)
			b := &blockState{typ: shape.Type, id: shape.ID, name: shape.Name, startRaw: f.ContentBlock}
			blocks[f.Index] = b
			order = append(order, f.Index)
			if shape.Type == "tool_use" {
				if !send(wire.Event{Type: wire.EventToolCallDelta, ToolCall: wire.ToolCallDelta{
					Index: f.Index, ID: shape.ID, Type: "function",
					Function: wire.ToolCallFuncDelta{Name: shape.Name},
				}}) {
					return true, nil
				}
			}

		case "content_block_delta":
			var f sseContentBlockDelta
			if err := json.Unmarshal([]byte(d), &f); err != nil {
				return false, err
			}
			b, ok := blocks[f.Index]
			if !ok {
				b = &blockState{}
				blocks[f.Index] = b
				order = append(order, f.Index)
			}
			switch f.Delta.Type {
			case "text_delta":
				b.text.WriteString(f.Delta.Text)
				if !send(wire.Event{Type: wire.EventContentDelta, Content: f.Delta.Text}) {
					return true, nil
				}
			case "thinking_delta":
				b.thinking.WriteString(f.Delta.Thinking)
				if !send(wire.Event{Type: wire.EventReasoningDelta, Reasoning: f.Delta.Thinking}) {
					return true, nil
				}
			case "signature_delta":
				b.signature = f.Delta.Signature
			case "input_json_delta":
				b.input.WriteString(f.Delta.PartialJSON)
				if b.typ == "tool_use" {
					if !send(wire.Event{Type: wire.EventToolCallDelta, ToolCall: wire.ToolCallDelta{
						Index: f.Index, Function: wire.ToolCallFuncDelta{Arguments: f.Delta.PartialJSON},
					}}) {
						return true, nil
					}
				}
			}

		case "content_block_stop":
			// Nothing to do: render() reads the block's accumulated state
			// at message_stop, once every block has one.

		case "message_delta":
			var f sseMessageDelta
			if err := json.Unmarshal([]byte(d), &f); err != nil {
				return false, err
			}
			stopReason = f.Delta.StopReason
			stopDetails = f.Delta.StopDetails
			deltaUsage = f.Usage

		case "message_stop":
			return true, nil

		case "error":
			var f apiErrorBody
			json.Unmarshal([]byte(d), &f)
			return true, &APIError{Type: f.Error.Type, Message: f.Error.Message, RequestID: f.RequestID}

		case "ping":
			// Keep-alive; nothing to do.
		}
		return false, nil
	}

	for {
		select {
		case <-ctx.Done():
			return streamResult{}, ctx.Err()
		case err := <-lineErrs:
			return streamResult{}, err
		case <-idle.C:
			return streamResult{}, ErrIdleTimeout
		case line, ok := <-lines:
			if !ok {
				return streamResult{}, errors.New("anthropic: stream ended without a message_stop frame")
			}
			if !idle.Stop() {
				<-idle.C
			}
			idle.Reset(c.idleTimeout())
			switch {
			case line == "":
				done, err := flush()
				if err != nil {
					return streamResult{}, err
				}
				if done {
					blocksOut := make([]json.RawMessage, 0, len(order))
					for _, idx := range order {
						blocksOut = append(blocksOut, blocks[idx].render())
					}
					return streamResult{
						blocks:      blocksOut,
						stopReason:  stopReason,
						stopDetails: stopDetails,
						usage:       mergeUsage(startUsage, deltaUsage),
					}, nil
				}
			case strings.HasPrefix(line, "event:"):
				event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				data += strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
			}
		}
	}
}
