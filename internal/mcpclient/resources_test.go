package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mrgeoffrich/agent-harness/internal/store"
	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// resourcefulServer advertises one resource, one resource template, and one
// prompt — the three things a probe reads beyond the tool list.
func resourcefulServer() *mcpsdk.Server {
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "resourceful", Version: "0.0.1"}, nil)
	server.AddTool(&mcpsdk.Tool{
		Name:        "noop",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	}, func(_ context.Context, _ *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		return &mcpsdk.CallToolResult{}, nil
	})
	server.AddResource(&mcpsdk.Resource{
		URI: "file:///notes.txt", Name: "notes", Description: "some notes", MIMEType: "text/plain",
	}, func(_ context.Context, _ *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
		return &mcpsdk.ReadResourceResult{Contents: []*mcpsdk.ResourceContents{
			{URI: "file:///notes.txt", MIMEType: "text/plain", Text: "the note itself"},
		}}, nil
	})
	server.AddResourceTemplate(&mcpsdk.ResourceTemplate{
		URITemplate: "file:///by-id/{id}", Name: "by id", Description: "one record",
	}, func(_ context.Context, _ *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
		return &mcpsdk.ReadResourceResult{}, nil
	})
	server.AddPrompt(&mcpsdk.Prompt{
		Name:        "review",
		Description: "review a diff",
		Arguments:   []*mcpsdk.PromptArgument{{Name: "diff", Description: "the diff", Required: true}},
	}, func(_ context.Context, req *mcpsdk.GetPromptRequest) (*mcpsdk.GetPromptResult, error) {
		return &mcpsdk.GetPromptResult{
			Description: "a review",
			Messages: []*mcpsdk.PromptMessage{
				{Role: "user", Content: &mcpsdk.TextContent{Text: "Review this: " + req.Params.Arguments["diff"]}},
			},
		}, nil
	})
	return server
}

// TestProbeSnapshotsResourcesAndPrompts pins that a probe reads all three
// lists, and that a template is stored as one rather than passing for a
// resource — the two are indistinguishable by URI, and a reader that could
// not tell them apart would eventually try to read a pattern.
func TestProbeSnapshotsResourcesAndPrompts(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateServer(t, s, store.MCPServer{Name: "srv", Transport: store.MCPTransportStdio, Command: "unused", Enabled: true})

	m := New(s)
	m.Dial = inMemoryTransportTo(t, resourcefulServer())

	got, err := m.Refresh(ctx, "srv")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if len(got.Resources) != 2 {
		t.Fatalf("expected the resource and the template, got %+v", got.Resources)
	}
	var resource, template store.MCPResourceSnapshot
	for _, r := range got.Resources {
		if r.Template {
			template = r
			continue
		}
		resource = r
	}
	if resource.URI != "file:///notes.txt" || resource.MIMEType != "text/plain" {
		t.Fatalf("resource = %+v, want the plain-text note", resource)
	}
	if template.URI != "file:///by-id/{id}" {
		t.Fatalf("template = %+v, want the {id} pattern", template)
	}
	if len(got.Prompts) != 1 || got.Prompts[0].Name != "review" {
		t.Fatalf("prompts = %+v, want the review prompt", got.Prompts)
	}
	if len(got.Prompts[0].Arguments) != 1 || !got.Prompts[0].Arguments[0].Required {
		t.Fatalf("prompt arguments = %+v, want one required argument", got.Prompts[0].Arguments)
	}
}

// TestProbeSkipsListsAServerDoesNotAdvertise is why the capability check
// exists: a tools-only server — which is most of them, the official Blender
// server included — answers resources/list with method-not-found, and a
// probe that asked anyway would turn every such server into a failed probe
// with a baffling reason.
func TestProbeSkipsListsAServerDoesNotAdvertise(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateServer(t, s, store.MCPServer{Name: "srv", Transport: store.MCPTransportStdio, Command: "unused", Enabled: true})

	m := New(s)
	m.Dial = inMemoryTransportTo(t, pagedTestServer(2))

	got, err := m.Refresh(ctx, "srv")
	if err != nil {
		t.Fatalf("Refresh should succeed against a tools-only server: %v", err)
	}
	if len(got.Tools) != 2 {
		t.Fatalf("expected the two tools, got %+v", got.Tools)
	}
	if len(got.Resources) != 0 || len(got.Prompts) != 0 {
		t.Fatalf("a tools-only server should contribute no resources or prompts, got %+v / %+v", got.Resources, got.Prompts)
	}
}

// TestReadResourceAndGetPrompt covers the two live paths.
func TestReadResourceAndGetPrompt(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateServer(t, s, store.MCPServer{Name: "srv", Transport: store.MCPTransportStdio, Command: "unused", Enabled: true})

	m := New(s)
	m.Dial = inMemoryTransportTo(t, resourcefulServer())
	t.Cleanup(func() { m.Close() })
	if _, err := m.Refresh(ctx, "srv"); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	res, err := m.ReadResource(ctx, "srv", "file:///notes.txt")
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if res.Text != "the note itself" {
		t.Fatalf("resource text = %q", res.Text)
	}

	prompt, err := m.GetPrompt(ctx, "srv", "review", map[string]string{"diff": "a-diff"})
	if err != nil {
		t.Fatalf("GetPrompt: %v", err)
	}
	for _, want := range []string{"a review", "[user]", "Review this: a-diff"} {
		if !strings.Contains(prompt.Text, want) {
			t.Errorf("prompt text should contain %q, got: %q", want, prompt.Text)
		}
	}
}

// TestReadResourceRefusesATemplate pins the refusal that matters most:
// reading a template literally may well succeed, returning the server's
// answer for a record called "{id}" — a wrong answer the model cannot tell
// from a right one.
func TestReadResourceRefusesATemplate(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateServer(t, s, store.MCPServer{Name: "srv", Transport: store.MCPTransportStdio, Command: "unused", Enabled: true})

	m := New(s)
	m.Dial = inMemoryTransportTo(t, resourcefulServer())
	t.Cleanup(func() { m.Close() })

	_, err := m.ReadResource(ctx, "srv", "file:///by-id/{id}")
	if err == nil || !strings.Contains(err.Error(), "template") {
		t.Fatalf("err = %v, want a refusal naming the template", err)
	}
}

// TestResourceAccessRefusesADisabledServer pins that the fixed tools cannot
// route around the enable toggle: a disabled server contributes nothing to
// a session, including by name.
func TestResourceAccessRefusesADisabledServer(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateServer(t, s, store.MCPServer{Name: "off", Transport: store.MCPTransportStdio, Command: "unused", Enabled: false})

	m := New(s)
	m.Dial = inMemoryTransportTo(t, resourcefulServer())
	t.Cleanup(func() { m.Close() })

	if _, err := m.ReadResource(ctx, "off", "file:///notes.txt"); err == nil {
		t.Fatal("expected reading from a disabled server to be refused")
	}
	if _, err := m.GetPrompt(ctx, "off", "review", nil); err == nil {
		t.Fatal("expected a prompt from a disabled server to be refused")
	}
}

// fakeSampler answers a sampling turn with fixed text, recording what it
// was asked.
type fakeSampler struct {
	intent wire.ChatIntent
	err    error
}

func (f *fakeSampler) CreateChatCompletion(_ context.Context, intent wire.ChatIntent) (*wire.ChatCompletionResponse, error) {
	f.intent = intent
	if f.err != nil {
		return nil, f.err
	}
	return &wire.ChatCompletionResponse{Choices: []wire.Choice{
		{Message: wire.Message{Role: wire.RoleAssistant, Content: wire.TextContent("the model's answer")}},
	}}, nil
}

// samplingServer asks the client to run a model turn, the way the current
// protocol requires: embedded in the result of the call it is serving, for
// the client's multi-round-trip middleware to fulfil.
func samplingServer() *mcpsdk.Server {
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "sampling-server", Version: "0.0.1"}, nil)
	server.AddTool(&mcpsdk.Tool{
		Name:        "ask",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	}, func(_ context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		if len(req.Params.InputResponses) == 0 {
			return &mcpsdk.CallToolResult{InputRequests: mcpsdk.InputRequestMap{"msg": &mcpsdk.CreateMessageParams{
				MaxTokens:    99999,
				SystemPrompt: "be brief",
				Messages: []*mcpsdk.SamplingMessage{
					{Role: "user", Content: &mcpsdk.TextContent{Text: "what is 2+2?"}},
				},
			}}}, nil
		}
		got := req.Params.InputResponses["msg"].(*mcpsdk.CreateMessageWithToolsResult)
		text := textOfContent(got.Content)
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "server saw: " + text}}}, nil
	})
	return server
}

func samplingFixture(t *testing.T, allow bool) (*Manager, *store.Store, *fakeSampler) {
	t.Helper()
	s := openTestStore(t)
	mustCreateServer(t, s, store.MCPServer{
		Name: "srv", Transport: store.MCPTransportStdio, Command: "unused",
		Enabled: true, AllowSampling: allow,
	})
	if err := s.SaveMCPProbe(context.Background(), "srv", store.MCPProbe{Tools: []store.MCPToolSnapshot{
		{Name: "ask", QualifiedName: "mcp__srv__ask", Description: "d"},
	}}, "", time.Now().UTC()); err != nil {
		t.Fatalf("save probe: %v", err)
	}
	sampler := &fakeSampler{}
	m := New(s)
	m.Dial = inMemoryTransportTo(t, samplingServer())
	m.Sampler = sampler
	t.Cleanup(func() { m.Close() })
	return m, s, sampler
}

// TestSamplingRunsAModelTurnForAnAllowedServer is the happy path, end to
// end: the server asks, the harness runs a turn, the server sees the answer.
func TestSamplingRunsAModelTurnForAnAllowedServer(t *testing.T) {
	m, _, sampler := samplingFixture(t, true)

	got, err := m.Call(context.Background(), "mcp__srv__ask", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got.Text != "server saw: the model's answer" {
		t.Fatalf("Text = %q, want the server's view of the sampled answer", got.Text)
	}

	// The server's own token request is a ceiling to lower, never an
	// instruction to follow.
	if sampler.intent.MaxTokens != maxSampleTokens {
		t.Errorf("MaxTokens = %d, want it capped at %d", sampler.intent.MaxTokens, maxSampleTokens)
	}
	if sampler.intent.Thinking {
		t.Error("a server's sampling turn should not be a thinking turn")
	}
	if len(sampler.intent.Messages) != 2 || sampler.intent.Messages[0].Role != wire.RoleSystem {
		t.Fatalf("messages = %+v, want the system prompt then the user turn", sampler.intent.Messages)
	}
}

// TestSamplingIsRefusedUnlessTheOperatorAllowedIt is the gate that matters:
// connecting a server and letting it spend the operator's tokens on prompts
// it wrote are separate decisions, and only one of them is implied by
// pressing Add.
func TestSamplingIsRefusedUnlessTheOperatorAllowedIt(t *testing.T) {
	m, _, sampler := samplingFixture(t, false)

	_, err := m.Call(context.Background(), "mcp__srv__ask", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected the call to fail once its sampling request was refused")
	}
	if !strings.Contains(err.Error(), "sampling is not enabled") {
		t.Fatalf("err = %v, want a refusal naming the missing permission", err)
	}
	if sampler.intent.Model != "" {
		t.Fatal("the model must not be called at all for a server that was never allowed to sample")
	}
}

// TestSamplingWithNoModelWiredSaysSo separates the two refusals: a Manager
// with no sampler cannot sample for anyone, which is a different fact from
// a server not being allowed to ask.
func TestSamplingWithNoModelWiredSaysSo(t *testing.T) {
	m, _, _ := samplingFixture(t, true)
	m.Sampler = nil

	_, err := m.Call(context.Background(), "mcp__srv__ask", json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "cannot sample") {
		t.Fatalf("err = %v, want a refusal naming the missing model", err)
	}
}

// TestSamplingReportsAModelFailure pins that a provider error reaches the
// server as an error rather than as an empty answer it cannot tell from a
// real one.
func TestSamplingReportsAModelFailure(t *testing.T) {
	m, _, sampler := samplingFixture(t, true)
	sampler.err = errors.New("upstream 500")

	_, err := m.Call(context.Background(), "mcp__srv__ask", json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "upstream 500") {
		t.Fatalf("err = %v, want the provider's failure carried through", err)
	}
}

// TestElicitationIsDeclined pins the policy for unattended runs: the server
// is told plainly that nobody answered, rather than being left to wait for
// an operator who is not there.
func TestElicitationIsDeclined(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateServer(t, s, store.MCPServer{Name: "srv", Transport: store.MCPTransportStdio, Command: "unused", Enabled: true})
	if err := s.SaveMCPProbe(ctx, "srv", store.MCPProbe{Tools: []store.MCPToolSnapshot{
		{Name: "ask_user", QualifiedName: "mcp__srv__ask_user", Description: "d"},
	}}, "", time.Now().UTC()); err != nil {
		t.Fatalf("save probe: %v", err)
	}

	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "elicit-server", Version: "0.0.1"}, nil)
	server.AddTool(&mcpsdk.Tool{
		Name:        "ask_user",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	}, func(_ context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		if len(req.Params.InputResponses) == 0 {
			return &mcpsdk.CallToolResult{InputRequests: mcpsdk.InputRequestMap{"q": &mcpsdk.ElicitParams{
				Message: "which city?",
			}}}, nil
		}
		got := req.Params.InputResponses["q"].(*mcpsdk.ElicitResult)
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "action=" + got.Action}}}, nil
	})

	m := New(s)
	m.Dial = inMemoryTransportTo(t, server)
	t.Cleanup(func() { m.Close() })

	got, err := m.Call(ctx, "mcp__srv__ask_user", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got.Text != "action=decline" {
		t.Fatalf("Text = %q, want the server told plainly that nobody answered", got.Text)
	}
}
