package session

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/deepseek"
	"github.com/mrgeoffrich/agent-harness/internal/store"
	"github.com/mrgeoffrich/agent-harness/internal/tools"
	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// scriptedClient answers each request with the next scripted event sequence.
// It embeds a real DeepSeek client for the rest of the seam (usage split,
// starvation predicate, argument repair), so only the stream itself is faked.
type scriptedClient struct {
	*deepseek.Client

	mu      sync.Mutex
	scripts [][]wire.Event
	calls   int
}

func (c *scriptedClient) StreamChatCompletion(ctx context.Context, intent wire.ChatIntent) (<-chan wire.Event, error) {
	c.mu.Lock()
	i := c.calls
	c.calls++
	c.mu.Unlock()
	if i >= len(c.scripts) {
		return nil, errors.New("scriptedClient: no script for this request")
	}
	out := make(chan wire.Event, len(c.scripts[i])+1)
	for _, ev := range c.scripts[i] {
		out <- ev
	}
	close(out)
	return out, nil
}

func scriptedRunner(t *testing.T, scripts ...[]wire.Event) *Runner {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &Runner{
		Store:      st,
		Mirror:     store.NewMirror(filepath.Join(dir, "mirror")),
		Client:     &scriptedClient{Client: deepseek.NewClient("http://unused.invalid", "test-key"), scripts: scripts},
		Prices:     testPrices(),
		FlashModel: "test-model",
	}
}

func runScripted(t *testing.T, r *Runner) (*RunResult, error) {
	t.Helper()
	return r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: t.TempDir(), PermissionMode: tools.ModeFull, Prompt: "write something",
	})
}

func usageOf(prompt, completion int) *wire.Usage {
	return &wire.Usage{PromptTokens: prompt, PromptCacheMissTokens: prompt, CompletionTokens: completion}
}

// A response the provider cut off at its token ceiling is not a finished
// answer, and the parent must be able to tell: the run's reason says so
// instead of reporting an ordinary no_tool_calls.
func TestRunThatEndsOnLengthReportsTruncation(t *testing.T) {
	r := scriptedRunner(t, []wire.Event{
		{Type: wire.EventContentDelta, Content: "unfinished sentence"},
		{Type: wire.EventFinish, FinishReason: wire.FinishLength},
		{Type: wire.EventUsage, Usage: usageOf(150, 10)},
	})
	res, err := runScripted(t, r)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Reason != ReasonTruncated {
		t.Fatalf("reason = %q, want %q", res.Reason, ReasonTruncated)
	}
	if res.Text != "unfinished sentence" {
		t.Fatalf("the partial text must still come back, got %q", res.Text)
	}
	if res.Status != store.StatusOK {
		t.Fatalf("the loop itself ended cleanly, status = %q", res.Status)
	}
}

func TestRunThatEndsOnContentFilterReportsIt(t *testing.T) {
	r := scriptedRunner(t, []wire.Event{
		{Type: wire.EventContentDelta, Content: "I can"},
		{Type: wire.EventFinish, FinishReason: wire.FinishContentFilter},
		{Type: wire.EventUsage, Usage: usageOf(150, 3)},
	})
	res, err := runScripted(t, r)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Reason != ReasonContentFilter {
		t.Fatalf("reason = %q, want %q", res.Reason, ReasonContentFilter)
	}
}

func TestRunThatEndsOnStopIsStillNoToolCalls(t *testing.T) {
	r := scriptedRunner(t, []wire.Event{
		{Type: wire.EventContentDelta, Content: "done"},
		{Type: wire.EventFinish, FinishReason: wire.FinishStop},
		{Type: wire.EventUsage, Usage: usageOf(150, 1)},
	})
	res, err := runScripted(t, r)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Reason != ReasonNoToolCalls {
		t.Fatalf("reason = %q, want %q", res.Reason, ReasonNoToolCalls)
	}
}

func eventKinds(events []store.Event) map[store.EventKind]int {
	n := map[store.EventKind]int{}
	for _, e := range events {
		n[e.Kind]++
	}
	return n
}

// Usage the provider reports before a request fails was billed, so it is
// committed even though nothing of the response is kept: no content and no
// turn_finished reach the log, so the history a resume replays is unchanged.
func TestUsageReportedBeforeAProviderErrorIsKept(t *testing.T) {
	r := scriptedRunner(t, []wire.Event{
		{Type: wire.EventContentDelta, Content: "half an ans"},
		{Type: wire.EventUsage, Usage: usageOf(150, 10)},
		{Type: wire.EventError, Err: errors.New("provider failed")},
	})
	res, err := runScripted(t, r)
	if err == nil {
		t.Fatal("expected the provider's error")
	}
	events, gerr := r.Store.GetEventsAfter(t.Context(), res.SessionID, 0, 0)
	if gerr != nil {
		t.Fatal(gerr)
	}
	usage := usageEvents(t, events)
	if len(usage) != 1 {
		t.Fatalf("expected the failed request's usage to be committed, got %d usage events", len(usage))
	}
	if usage[0].PromptTokens != 150 || usage[0].CompletionTokens != 10 || usage[0].SubTurn != 1 {
		t.Fatalf("usage = %+v", usage[0])
	}
	if usage[0].CostUSD <= 0 {
		t.Fatal("expected the failed request to carry a cost")
	}
	kinds := eventKinds(events)
	if kinds[store.KindContentDelta] != 0 || kinds[store.KindTurnFinished] != 0 {
		t.Fatalf("an incomplete response must not enter the log, got %v", kinds)
	}
	if res.Usage.CompletionTokens != 10 {
		t.Fatalf("the run's usage must include the failed request, got %+v", res.Usage)
	}
}

// A provider that fails before reporting any usage leaves nothing to record.
func TestAProviderErrorWithoutUsageCommitsNone(t *testing.T) {
	r := scriptedRunner(t, []wire.Event{{Type: wire.EventError, Err: errors.New("provider failed")}})
	res, err := runScripted(t, r)
	if err == nil {
		t.Fatal("expected the provider's error")
	}
	events, gerr := r.Store.GetEventsAfter(t.Context(), res.SessionID, 0, 0)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if n := len(usageEvents(t, events)); n != 0 {
		t.Fatalf("expected no usage events, got %d", n)
	}
}

// The first, reasoning-starved attempt succeeded as a request and was billed;
// when the retry then fails, that spend must still reach the log, beside what
// the retry reported before it failed.
func TestStarvedAttemptUsageSurvivesAFailedRetry(t *testing.T) {
	r := scriptedRunner(t,
		[]wire.Event{
			{Type: wire.EventReasoningDelta, Reasoning: "thinking"},
			{Type: wire.EventFinish, FinishReason: wire.FinishLength},
			{Type: wire.EventUsage, Usage: usageOf(200, 4000)},
		},
		[]wire.Event{
			{Type: wire.EventUsage, Usage: usageOf(200, 50)},
			{Type: wire.EventError, Err: errors.New("retry failed")},
		},
	)
	res, err := runScripted(t, r)
	if err == nil {
		t.Fatal("expected the retry's error")
	}
	events, gerr := r.Store.GetEventsAfter(t.Context(), res.SessionID, 0, 0)
	if gerr != nil {
		t.Fatal(gerr)
	}
	usage := usageEvents(t, events)
	if len(usage) != 2 {
		t.Fatalf("expected both requests' usage, got %d events", len(usage))
	}
	if usage[0].Attempt != 1 || usage[0].CompletionTokens != 4000 {
		t.Fatalf("first attempt = %+v", usage[0])
	}
	if usage[1].Attempt != 2 || usage[1].CompletionTokens != 50 {
		t.Fatalf("retry = %+v", usage[1])
	}
}
