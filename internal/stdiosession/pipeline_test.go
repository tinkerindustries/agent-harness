package stdiosession

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/hub"
	"github.com/mrgeoffrich/agent-harness/internal/session"
	"github.com/mrgeoffrich/agent-harness/internal/store"
)

// These tests start from what the loop commits and assert what a parent
// receives, because the failures they guard — a field dropped or distorted at
// a boundary — are invisible to a test that looks at either side alone.

type frame struct {
	method string
	params any
}

type capture struct{ frames []frame }

func (c *capture) emit(method string, params any) {
	c.frames = append(c.frames, frame{method, params})
}

func (c *capture) all() string {
	var b strings.Builder
	for _, f := range c.frames {
		raw, _ := json.Marshal(f.params)
		b.WriteString(f.method + " " + string(raw) + "\n")
	}
	return b.String()
}

func committed(t *testing.T, kind store.EventKind, payload any) store.Event {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return store.Event{Kind: kind, Payload: raw}
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A stored request of 150 prompt tokens: 20 read from the cache, 30 ordinary,
// 100 written to it. Claude's cache-miss figure leaves writes out on purpose,
// so a total rebuilt from hit and miss alone loses them.
var cacheWriteUsage = store.UsagePayload{
	SubTurn: 1, PromptTokens: 150,
	PromptCacheHitTokens: 20, PromptCacheMissTokens: 30, PromptCacheWriteTokens: 100,
	CompletionTokens: 10,
}

func TestResponsesUsageCountsCacheWrites(t *testing.T) {
	u := usageFrom(cacheWriteUsage)
	if u.InputTokens != 150 || u.TotalTokens != 160 {
		t.Fatalf("input %d total %d, want 150 and 160", u.InputTokens, u.TotalTokens)
	}
	if u.InputTokensDetails.CachedTokens != 20 {
		t.Fatalf("cached_tokens = %d, want 20", u.InputTokensDetails.CachedTokens)
	}
	if u.Harness == nil || u.Harness.CacheWriteTokens != 100 {
		t.Fatalf("harness = %+v, want cache_write_tokens 100", u.Harness)
	}

	// The running total a response carries sums them too.
	tr := newTranslator("resp", "m", (&capture{}).emit)
	tr.Event(committed(t, store.KindUsage, cacheWriteUsage))
	tr.Event(committed(t, store.KindUsage, cacheWriteUsage))
	_, total := tr.snapshot()
	if total.InputTokens != 300 || total.Harness.CacheWriteTokens != 200 {
		t.Fatalf("total = %+v / %+v", total, total.Harness)
	}
}

func TestManagedAgentsUsageCountsCacheWrites(t *testing.T) {
	u := maUsageFrom(cacheWriteUsage)
	if u.InputTokens != 150 || u.TotalTokens != 160 {
		t.Fatalf("input %d total %d, want 150 and 160", u.InputTokens, u.TotalTokens)
	}
	if u.CacheReadInputTokens != 20 || u.CacheCreationInputTokens != 100 {
		t.Fatalf("cache read %d creation %d, want 20 and 100", u.CacheReadInputTokens, u.CacheCreationInputTokens)
	}
}

func TestInteractionsUsageCountsCacheWrites(t *testing.T) {
	u := iactUsageFrom(cacheWriteUsage)
	if u.TotalInputTokens != 150 || u.TotalTokens != 160 {
		t.Fatalf("input %d total %d, want 150 and 160", u.TotalInputTokens, u.TotalTokens)
	}
}

// A provider that records no cache writes reports exactly what it did before.
func TestUsageWithoutCacheWritesIsUnchanged(t *testing.T) {
	p := store.UsagePayload{PromptTokens: 50, PromptCacheHitTokens: 20, PromptCacheMissTokens: 30, CompletionTokens: 10}
	if u := usageFrom(p); u.InputTokens != 50 || u.TotalTokens != 60 {
		t.Fatalf("usage = %+v", u)
	}
	if got := jsonOf(t, usageFrom(p)); strings.Contains(got, "cache_write") {
		t.Fatalf("an absent write count must not appear: %s", got)
	}
}

// A returned snapshot is serialised after the translator's lock is released,
// while the stream goes on writing, so it must not share anything with the
// live document.
func TestResponsesSnapshotIsIndependentOfLaterDeltas(t *testing.T) {
	tr := newTranslator("resp", "m", (&capture{}).emit)
	tr.Live(hub.LiveDelta{Channel: hub.ChannelContent, Text: "before"})
	tr.Event(committed(t, store.KindUsage, store.UsagePayload{PromptCacheMissTokens: 10, CompletionTokens: 1, CostUSD: 1}))
	items, usage := tr.snapshot()
	frozen := jsonOf(t, items) + jsonOf(t, usage)

	tr.Live(hub.LiveDelta{Channel: hub.ChannelContent, Text: " after"})
	tr.Event(committed(t, store.KindUsage, store.UsagePayload{PromptCacheMissTokens: 10, CompletionTokens: 1, CostUSD: 1}))
	tr.Event(committed(t, store.KindToolCall, store.ToolCallPayload{ID: "c", Name: "n", Arguments: `{"a":1}`}))

	if got := jsonOf(t, items) + jsonOf(t, usage); got != frozen {
		t.Fatalf("snapshot changed after it was returned:\n before %s\n after  %s", frozen, got)
	}
}

func TestResponsesSnapshotKeepsFunctionArgumentsStable(t *testing.T) {
	tr := newTranslator("resp", "m", (&capture{}).emit)
	idx := tr.addItem(OutputItem{Type: ItemFunctionCall, CallID: "c", Name: "n", Arguments: toolArguments("{}")})
	tr.delta(NotifyFunctionCallArgsDelta, idx, `{"a":`)
	items, _ := tr.snapshot()
	frozen := jsonOf(t, items)
	tr.delta(NotifyFunctionCallArgsDelta, idx, `1}`)
	if got := jsonOf(t, items); got != frozen {
		t.Fatalf("snapshot arguments changed: %s -> %s", frozen, got)
	}
}

func TestInteractionsAndManagedAgentsSnapshotsAreIndependent(t *testing.T) {
	it := newIactTranslator("int", "m", (&capture{}).emit)
	it.Live(hub.LiveDelta{Channel: hub.ChannelContent, Text: "before"})
	steps, _ := it.snapshot()
	frozen := jsonOf(t, steps)
	it.Live(hub.LiveDelta{Channel: hub.ChannelContent, Text: " after"})
	if got := jsonOf(t, steps); got != frozen {
		t.Fatalf("interactions snapshot changed: %s -> %s", frozen, got)
	}

	ma := newMATranslator("turn", "sess", "m", (&capture{}).emit)
	ma.Event(committed(t, store.KindToolCall, store.ToolCallPayload{ID: "c", Name: "n", Arguments: `{"a":1}`}))
	events, usage := ma.snapshot()
	frozen = jsonOf(t, events) + jsonOf(t, usage)
	ma.Event(committed(t, store.KindUsage, store.UsagePayload{PromptCacheMissTokens: 10, CompletionTokens: 1, CostUSD: 1}))
	if got := jsonOf(t, events) + jsonOf(t, usage); got != frozen {
		t.Fatalf("managed agents snapshot changed: %s -> %s", frozen, got)
	}
}

// The reasoning-starved retry streams a second attempt's reasoning into the
// item the discarded attempt opened. Only the accepted attempt's reasoning is
// committed, and the assembled response must match the log.
func TestResponsesRetryReasoningIsReconciled(t *testing.T) {
	c := &capture{}
	tr := newTranslator("resp", "m", c.emit)
	tr.Live(hub.LiveDelta{Channel: hub.ChannelReasoning, Text: "discarded attempt;"})
	tr.Live(hub.LiveDelta{Channel: hub.ChannelReasoning, Text: "accepted attempt"})
	tr.Event(committed(t, store.KindReasoningDelta, store.ReasoningDeltaPayload{Text: "accepted attempt"}))

	items, _ := tr.snapshot()
	if len(items) != 1 || len(items[0].Content) != 1 || items[0].Content[0].Text != "accepted attempt" {
		t.Fatalf("assembled reasoning = %s", jsonOf(t, items))
	}
	last := c.frames[len(c.frames)-1]
	if last.method != NotifyOutputItemDone || !strings.Contains(jsonOf(t, last.params), `"text":"accepted attempt"`) ||
		strings.Contains(jsonOf(t, last.params), "discarded") {
		t.Fatalf("output_item.done must carry the accepted reasoning alone, got %s %s", last.method, jsonOf(t, last.params))
	}
}

func TestResponsesLiveTextThatMatchesTheLogIsLeftAlone(t *testing.T) {
	tr := newTranslator("resp", "m", (&capture{}).emit)
	tr.Live(hub.LiveDelta{Channel: hub.ChannelContent, Text: "hel"})
	tr.Live(hub.LiveDelta{Channel: hub.ChannelContent, Text: "lo"})
	tr.Event(committed(t, store.KindContentDelta, store.ContentDeltaPayload{Text: "hello"}))
	items, _ := tr.snapshot()
	if len(items[0].Content) != 1 || items[0].Content[0].Text != "hello" {
		t.Fatalf("content = %s", jsonOf(t, items))
	}
}

func TestInteractionsRetryReasoningIsReconciled(t *testing.T) {
	tr := newIactTranslator("int", "m", (&capture{}).emit)
	tr.Live(hub.LiveDelta{Channel: hub.ChannelReasoning, Text: "discarded attempt;"})
	tr.Live(hub.LiveDelta{Channel: hub.ChannelReasoning, Text: "accepted attempt"})
	tr.Event(committed(t, store.KindReasoningDelta, store.ReasoningDeltaPayload{Text: "accepted attempt"}))
	steps, _ := tr.snapshot()
	if len(steps) != 1 || len(steps[0].Summary) != 1 || steps[0].Summary[0].Text != "accepted attempt" {
		t.Fatalf("assembled summary = %s", jsonOf(t, steps))
	}
}

func TestManagedAgentsRetryReasoningIsReconciled(t *testing.T) {
	c := &capture{}
	tr := newMATranslator("turn", "sess", "m", c.emit)
	tr.Live(hub.LiveDelta{Channel: hub.ChannelReasoning, Text: "discarded attempt;"})
	tr.Live(hub.LiveDelta{Channel: hub.ChannelReasoning, Text: "accepted attempt"})
	tr.Event(committed(t, store.KindReasoningDelta, store.ReasoningDeltaPayload{Text: "accepted attempt"}))
	events, _ := tr.snapshot()
	if len(events) != 1 || len(events[0].Thinking) != 1 || events[0].Thinking[0].Text != "accepted attempt" {
		t.Fatalf("recorded thinking = %s", jsonOf(t, events))
	}
}

// What Claude's adapter stores for an answer that used a server-side search:
// the call, its result (encrypted content included, for replay), and an answer
// in two text blocks, the first cited.
const citedBlocks = `[
 {"type":"thinking","thinking":"plan","signature":"SIGNATURE-BLOB"},
 {"type":"server_tool_use","id":"srv_1","name":"web_search","input":{"query":"go release"}},
 {"type":"web_search_tool_result","tool_use_id":"srv_1","content":[
   {"type":"web_search_result","url":"https://go.dev/doc/devel/release","title":"Release History","page_age":"1 day","encrypted_content":"ENCRYPTED-BLOB"}]},
 {"type":"text","text":"Go 1.24 shipped. ","citations":[
   {"type":"web_search_result_location","url":"https://go.dev/doc/devel/release","title":"Release History","cited_text":"go1.24 (released 2025-02-11)","encrypted_index":"INDEX-BLOB"}]},
 {"type":"text","text":"More is coming."}
]`

func TestResponsesCitationsAndServerToolsReachTheParent(t *testing.T) {
	c := &capture{}
	tr := newTranslator("resp", "m", c.emit)
	const answer = "Go 1.24 shipped. More is coming."
	tr.Live(hub.LiveDelta{Channel: hub.ChannelContent, Text: answer})
	tr.Event(committed(t, store.KindReasoningDelta, store.ReasoningDeltaPayload{
		Text: "plan", ProviderBlocks: json.RawMessage(citedBlocks),
	}))
	tr.Event(committed(t, store.KindContentDelta, store.ContentDeltaPayload{Text: answer}))

	items, _ := tr.snapshot()
	var call, message *OutputItem
	for i := range items {
		switch items[i].Type {
		case ItemServerToolCall:
			call = &items[i]
		case ItemMessage:
			message = &items[i]
		}
	}
	if call == nil || call.CallID != "srv_1" || call.Name != "web_search" ||
		string(call.Arguments) != `{"query":"go release"}` {
		t.Fatalf("server tool call = %s", jsonOf(t, items))
	}
	if call.Status != StatusCompleted || call.Harness == nil || len(call.Harness.Sources) != 1 ||
		call.Harness.Sources[0].URL != "https://go.dev/doc/devel/release" {
		t.Fatalf("server tool sources = %s", jsonOf(t, call))
	}
	if message == nil || len(message.Content) != 1 || len(message.Content[0].Annotations) != 1 {
		t.Fatalf("message = %s", jsonOf(t, message))
	}
	a := message.Content[0].Annotations[0]
	if a.Type != "url_citation" || a.URL != "https://go.dev/doc/devel/release" || a.Title != "Release History" ||
		a.StartIndex != 0 || a.EndIndex != len("Go 1.24 shipped. ") || a.CitedText != "go1.24 (released 2025-02-11)" {
		t.Fatalf("annotation = %+v", a)
	}

	// Replay material stays internal.
	out := c.all() + jsonOf(t, items)
	for _, secret := range []string{"ENCRYPTED-BLOB", "INDEX-BLOB", "SIGNATURE-BLOB"} {
		if strings.Contains(out, secret) {
			t.Fatalf("%s reached the parent:\n%s", secret, out)
		}
	}
}

// Offsets into text the blocks do not describe would point at the wrong
// characters, so a mismatch attaches nothing rather than something wrong.
func TestResponsesCitationsAreDroppedWhenTheTextDoesNotMatch(t *testing.T) {
	tr := newTranslator("resp", "m", (&capture{}).emit)
	tr.Event(committed(t, store.KindReasoningDelta, store.ReasoningDeltaPayload{ProviderBlocks: json.RawMessage(citedBlocks)}))
	tr.Event(committed(t, store.KindContentDelta, store.ContentDeltaPayload{Text: "something else entirely"}))
	items, _ := tr.snapshot()
	for _, it := range items {
		for _, p := range it.Content {
			if len(p.Annotations) != 0 {
				t.Fatalf("annotations attached to unrelated text: %s", jsonOf(t, items))
			}
		}
	}
}

func TestResponsesCitationOffsetsCountCharacters(t *testing.T) {
	blocks := `[{"type":"text","text":"héllo ","citations":[{"type":"web_search_result_location","url":"https://a.test","title":"A"}]},
	             {"type":"text","text":"wörld","citations":[{"type":"web_search_result_location","url":"https://b.test","title":"B"}]}]`
	tr := newTranslator("resp", "m", (&capture{}).emit)
	tr.Event(committed(t, store.KindReasoningDelta, store.ReasoningDeltaPayload{ProviderBlocks: json.RawMessage(blocks)}))
	tr.Event(committed(t, store.KindContentDelta, store.ContentDeltaPayload{Text: "héllo wörld"}))
	items, _ := tr.snapshot()
	notes := items[len(items)-1].Content[0].Annotations
	if len(notes) != 2 || notes[0].StartIndex != 0 || notes[0].EndIndex != 6 || notes[1].StartIndex != 6 || notes[1].EndIndex != 11 {
		t.Fatalf("annotations = %+v", notes)
	}
}

func TestManagedAgentsCitationsAndServerToolsReachTheParent(t *testing.T) {
	c := &capture{}
	tr := newMATranslator("turn", "sess", "m", c.emit)
	const answer = "Go 1.24 shipped. More is coming."
	tr.Live(hub.LiveDelta{Channel: hub.ChannelContent, Text: answer})
	tr.Event(committed(t, store.KindReasoningDelta, store.ReasoningDeltaPayload{
		Text: "plan", ProviderBlocks: json.RawMessage(citedBlocks),
	}))
	tr.Event(committed(t, store.KindContentDelta, store.ContentDeltaPayload{Text: answer}))

	events, _ := tr.snapshot()
	var use, result, message *maEventOut
	for i := range events {
		switch events[i].Type {
		case notifyMAAgentToolUse:
			use = &events[i]
		case notifyMAAgentToolResult:
			result = &events[i]
		case notifyMAAgentMessage:
			message = &events[i]
		}
	}
	if use == nil || use.ID != "srv_1" || use.Name != "web_search" || use.Harness == nil || !use.Harness.ServerTool {
		t.Fatalf("server tool use = %s", jsonOf(t, events))
	}
	if result == nil || result.ToolUseID != "srv_1" || result.Harness == nil || !result.Harness.ServerTool ||
		len(result.Harness.Sources) != 1 || result.IsError {
		t.Fatalf("server tool result = %s", jsonOf(t, events))
	}
	if message == nil || len(message.Content) != 2 || len(message.Content[0].Citations) != 1 || len(message.Content[1].Citations) != 0 {
		t.Fatalf("message = %s", jsonOf(t, message))
	}
	cite := message.Content[0].Citations[0]
	if cite.URL != "https://go.dev/doc/devel/release" || cite.CitedText != "go1.24 (released 2025-02-11)" {
		t.Fatalf("citation = %+v", cite)
	}
	// A server tool is not a custom tool: nothing is owed to it.
	if got := tr.pendingCustomToolIDs(); len(got) != 0 {
		t.Fatalf("a server tool left a custom tool pending: %v", got)
	}
	out := c.all() + jsonOf(t, events)
	for _, secret := range []string{"ENCRYPTED-BLOB", "INDEX-BLOB", "SIGNATURE-BLOB"} {
		if strings.Contains(out, secret) {
			t.Fatalf("%s reached the parent:\n%s", secret, out)
		}
	}
}

func TestAFailedServerToolIsReportedAsAnError(t *testing.T) {
	blocks := `[{"type":"server_tool_use","id":"srv_2","name":"web_fetch","input":{"url":"https://x.test"}},
	             {"type":"web_fetch_tool_result","tool_use_id":"srv_2","content":{"type":"web_fetch_tool_error","error_code":"url_not_accessible"}}]`
	tr := newMATranslator("turn", "sess", "m", (&capture{}).emit)
	tr.Event(committed(t, store.KindReasoningDelta, store.ReasoningDeltaPayload{ProviderBlocks: json.RawMessage(blocks)}))
	events, _ := tr.snapshot()
	var result *maEventOut
	for i := range events {
		if events[i].Type == notifyMAAgentToolResult {
			result = &events[i]
		}
	}
	if result == nil || !result.IsError || result.Harness.ToolError != "url_not_accessible" {
		t.Fatalf("result = %s", jsonOf(t, events))
	}
}

// A response with no provider blocks, which is every response from every
// other provider, produces exactly what it did before.
func TestResponsesWithoutProviderBlocksAddNothing(t *testing.T) {
	tr := newTranslator("resp", "m", (&capture{}).emit)
	tr.Event(committed(t, store.KindReasoningDelta, store.ReasoningDeltaPayload{Text: "thinking"}))
	tr.Event(committed(t, store.KindContentDelta, store.ContentDeltaPayload{Text: "answer"}))
	items, _ := tr.snapshot()
	if len(items) != 2 || items[0].Type != ItemReasoning || items[1].Type != ItemMessage {
		t.Fatalf("items = %s", jsonOf(t, items))
	}
	if strings.Contains(jsonOf(t, items), "annotations") {
		t.Fatalf("unexpected annotations: %s", jsonOf(t, items))
	}
}

// A run the provider cut short is reported as incomplete, never as completed,
// and its reason says which way.
func TestTruncatedRunsAreIncomplete(t *testing.T) {
	cases := []struct {
		reason string
		status string
		stop   string
	}{
		{session.ReasonNoToolCalls, StatusCompleted, "end_turn"},
		{session.ReasonTruncated, StatusIncomplete, "truncated"},
		{session.ReasonContentFilter, StatusIncomplete, "content_filter"},
		{"complete", StatusCompleted, "end_turn"},
	}
	for _, c := range cases {
		res := &session.RunResult{Status: store.StatusOK, Reason: c.reason}
		if got := statusFor(res); got != c.status {
			t.Errorf("statusFor(%s) = %s, want %s", c.reason, got, c.status)
		}
		if got := maStopReasonType(c.reason); got != c.stop {
			t.Errorf("maStopReasonType(%s) = %s, want %s", c.reason, got, c.stop)
		}
	}
	if got := statusFor(&session.RunResult{Status: store.StatusFailed, Reason: session.ReasonTruncated}); got != StatusFailed {
		t.Errorf("a failed run stays failed, got %s", got)
	}
}
