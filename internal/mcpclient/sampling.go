package mcpclient

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mrgeoffrich/agent-harness/internal/store"
	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// Sampling and elicitation are the two places where a server asks the
// client for something rather than answering it (docs/MCP.md, "Sampling"
// and "Elicitation"). Since protocol version 2026-07-28 neither arrives as
// a standalone request: a server embeds the ask in the result of the call
// it is already serving, and the SDK's multi-round-trip middleware fulfils
// it from the handlers below and re-invokes the server's handler with the
// answer (SEP-2322). So there is no retry loop here to write — only the two
// handlers, and the policy each applies.

// maxSampleTokens caps a single sampling turn regardless of what the server
// asked for. A server naming its own token budget is a server spending
// somebody else's money, so its request is treated as a ceiling to be
// lowered, never as an instruction.
const maxSampleTokens = 4000

// SamplingClient is the narrow seam a sampling turn runs through: one
// non-streaming completion, the same shape internal/tools uses for its own
// side work. Declared here where it is consumed, implemented by the
// provider client cmd/harness already builds.
type SamplingClient interface {
	CreateChatCompletion(ctx context.Context, intent wire.ChatIntent) (*wire.ChatCompletionResponse, error)
}

// createMessage answers a server's sampling request (`sampling/createMessage`).
//
// Two gates stand in front of it, and they are different questions. The
// first is whether this Manager can sample at all — no client wired, the
// shape every existing test and a caller with none wired take, means no.
// The second is whether this particular server is allowed to: AllowSampling
// is off by default, because connecting a server and letting it spend the
// operator's tokens on prompts it wrote are separate decisions, and only one
// of them is implied by pressing Add.
//
// A refusal is an error rather than an empty completion. The server asked
// for something it did not get, and telling it so lets it fall back;
// answering with silence dressed as a model turn would be a lie it cannot
// detect.
func (m *Manager) createMessage(ctx context.Context, srv store.MCPServer, req *mcpsdk.CreateMessageWithToolsRequest) (*mcpsdk.CreateMessageResult, error) {
	if m.Sampler == nil {
		return nil, errors.New("this client cannot sample: no model is wired to it")
	}
	if !srv.AllowSampling {
		return nil, fmt.Errorf("sampling is not enabled for the %q MCP server: an operator must turn it on", srv.Name)
	}
	if req == nil || req.Params == nil || len(req.Params.Messages) == 0 {
		return nil, errors.New("sampling request carried no messages")
	}

	model := m.SamplingModel
	if model == "" {
		model = defaultSamplingModel
	}
	items := make([]wire.Item, 0, len(req.Params.Messages)+1)
	if sys := strings.TrimSpace(req.Params.SystemPrompt); sys != "" {
		items = append(items, wire.SystemItem(sys))
	}
	for _, msg := range req.Params.Messages {
		text := textOfContent(msg.Content)
		if text == "" {
			continue
		}
		if msg.Role == "assistant" {
			items = append(items, wire.AssistantItem(text))
			continue
		}
		items = append(items, wire.UserItem(text))
	}
	if len(items) == 0 {
		return nil, errors.New("sampling request carried no text this client can send")
	}

	maxTokens := maxSampleTokens
	if req.Params.MaxTokens > 0 && int(req.Params.MaxTokens) < maxTokens {
		maxTokens = int(req.Params.MaxTokens)
	}

	log.Printf("mcpclient: %s: sampling %d message(s) on model %s", srv.Name, len(items), model)
	resp, err := m.Sampler.CreateChatCompletion(ctx, wire.ChatIntent{
		Model: model,
		Items: items,
		// Off for the same reason internal/tools disables it on side work:
		// a server wants an answer, not the reasoning that produced it, and
		// non-thinking turns are cheaper (docs/MODELS.md).
		Thinking:  false,
		MaxTokens: maxTokens,
	})
	if err != nil {
		return nil, fmt.Errorf("sampling for %q: %w", srv.Name, err)
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("sampling for %q: the model returned no choices", srv.Name)
	}

	return &mcpsdk.CreateMessageResult{
		Model:      model,
		Role:       "assistant",
		Content:    &mcpsdk.TextContent{Text: resp.Choices[0].Message.Content.String()},
		StopReason: "endTurn",
	}, nil
}

// elicit answers a server's request to put a question to the user
// (`elicitation/create`).
//
// It always declines, and that is the design rather than a stub. Work here
// arrives on a durable queue and runs unattended — the operator who
// submitted it is not sitting in front of a form, and may not be awake. The
// protocol has a word for exactly this situation, so the honest answer is
// "decline": the server learns nobody answered and can take its other path,
// where blocking would hang a queued run behind a question no one will ever
// see.
//
// The ask is logged in full, because a server asking for input is telling
// the operator something about how it expects to be driven, and that is
// worth being able to read after the fact.
func (m *Manager) elicit(_ context.Context, srv store.MCPServer, req *mcpsdk.ElicitRequest) (*mcpsdk.ElicitResult, error) {
	message := ""
	if req != nil && req.Params != nil {
		message = req.Params.Message
	}
	log.Printf("mcpclient: %s: declined an elicitation (runs here are unattended): %s", srv.Name, message)
	return &mcpsdk.ElicitResult{Action: "decline"}, nil
}

// textOfContent flattens one sampling message's content blocks to text.
// Images and audio a server sends for the model to look at are dropped
// rather than described: this client's sampling path is a text completion,
// and inventing a caption for a picture nobody looked at would be worse
// than saying less.
func textOfContent(blocks []mcpsdk.Content) string {
	var b strings.Builder
	for _, c := range blocks {
		if t, ok := c.(*mcpsdk.TextContent); ok {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(t.Text)
		}
	}
	return b.String()
}
