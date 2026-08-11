package session

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mrgeoffrich/deepseek-harness/internal/deepseek"
	"github.com/mrgeoffrich/deepseek-harness/internal/hub"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
	"github.com/mrgeoffrich/deepseek-harness/internal/tools"
)

// slowDeltaServer answers with reasoning and content split into chunks,
// pausing between them so the response spans real time — the shape a
// streamed response has, and what makes the timestamp assertions below mean
// anything.
func slowDeltaServer(t *testing.T, reasoning, content []string, gap time.Duration) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for _, chunk := range reasoning {
			writeSSEChunk(t, w, deepseek.ChatCompletionChunk{
				Choices: []deepseek.ChunkChoice{{Delta: deepseek.ChunkDelta{Role: "assistant", ReasoningContent: strPtr(chunk)}}},
			})
			flusher.Flush()
			time.Sleep(gap)
		}
		for _, chunk := range content {
			writeSSEChunk(t, w, deepseek.ChatCompletionChunk{
				Choices: []deepseek.ChunkChoice{{Delta: deepseek.ChunkDelta{Content: strPtr(chunk)}}},
			})
			flusher.Flush()
			time.Sleep(gap)
		}
		writeSSEChunk(t, w, deepseek.ChatCompletionChunk{
			Choices: []deepseek.ChunkChoice{{Delta: deepseek.ChunkDelta{}, FinishReason: strPtr(deepseek.FinishStop)}},
			Usage:   &deepseek.Usage{PromptTokens: 100, PromptCacheHitTokens: 50, PromptCacheMissTokens: 50, CompletionTokens: 5},
		})
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
}

// turn_started is committed on its own, ahead of the request, so its
// created_at is when the sub-turn began rather than when the model finished.
// AppendEvents stamps one instant across a batch, so committing it with the
// rest gave the transcript's live turn nothing to measure its age from — it
// always read ~0 (docs/reviews/sess-bb6c0ed564ddae573c3b1832cb3981f4.md).
// Asserted as a real gap, because sharing one instant was the bug.
func TestTurnStartedIsStampedBeforeTheModelResponds(t *testing.T) {
	srv := slowDeltaServer(t, []string{"thinking"}, []string{"done"}, 150*time.Millisecond)
	defer srv.Close()
	r := newTestRunner(t, srv.URL)

	res, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: deepseek.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: t.TempDir(), PermissionMode: tools.ModeFull, Prompt: "go",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	events, err := r.Store.GetEvents(t.Context(), res.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var started, finished time.Time
	var startedSeq, reasoningSeq int64
	for _, ev := range events {
		switch ev.Kind {
		case store.KindTurnStarted:
			if started.IsZero() {
				started, startedSeq = ev.CreatedAt, ev.Seq
			}
		case store.KindReasoningDelta:
			if reasoningSeq == 0 {
				reasoningSeq = ev.Seq
			}
		case store.KindTurnFinished:
			if finished.IsZero() {
				finished = ev.CreatedAt
			}
		}
	}
	if started.IsZero() || finished.IsZero() {
		t.Fatal("expected both a turn_started and a turn_finished")
	}
	if gap := finished.Sub(started); gap < 100*time.Millisecond {
		t.Fatalf("turn_started and turn_finished are %v apart; turn_started is still being stamped with the batch", gap)
	}
	if startedSeq >= reasoningSeq {
		t.Fatalf("turn_started (seq %d) must precede the deltas (seq %d)", startedSeq, reasoningSeq)
	}
}

// Streaming must not change what the log holds: the committed events still
// carry the response exactly once. Getting this wrong in the other direction
// — folding the live text into the same field the committed events append to
// — is what would double every streamed sub-turn.
func TestLiveStreamingLeavesTheCommittedLogIntact(t *testing.T) {
	srv := slowDeltaServer(t,
		[]string{"weigh", "ing the ", "options"},
		[]string{"here is ", "the answer"},
		40*time.Millisecond)
	defer srv.Close()

	r := newTestRunner(t, srv.URL)
	r.Hub = hub.New()

	res, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: deepseek.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: t.TempDir(), PermissionMode: tools.ModeFull, Prompt: "think out loud",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	events, err := r.Store.GetEvents(t.Context(), res.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var reasoning, content string
	for _, ev := range events {
		switch ev.Kind {
		case store.KindReasoningDelta:
			var p store.ReasoningDeltaPayload
			if err := json.Unmarshal(ev.Payload, &p); err != nil {
				t.Fatal(err)
			}
			reasoning += p.Text
		case store.KindContentDelta:
			var p store.ContentDeltaPayload
			if err := json.Unmarshal(ev.Payload, &p); err != nil {
				t.Fatal(err)
			}
			content += p.Text
		}
	}
	if reasoning != "weighing the options" {
		t.Fatalf("committed reasoning = %q, want the whole text exactly once", reasoning)
	}
	if content != "here is the answer" {
		t.Fatalf("committed content = %q, want the whole text exactly once", content)
	}
}

// What a watching subscriber actually receives. Driving the sink directly
// rather than through Run avoids racing to subscribe to a session id that
// is minted inside the run, and the sink is the unit that decides both the
// coalescing and the frame shape.
func TestLiveSinkCoalescesAndPublishes(t *testing.T) {
	h := hub.New()
	sink := newLiveSink(h, "sess-live", 3)
	ch, cancel := h.Subscribe("sess-live")
	defer cancel()

	// Adds inside one interval accumulate rather than publishing: a frame
	// per token would put thousands through a channel that drops its
	// subscriber at 256.
	sink.add(hub.ChannelReasoning, "we")
	sink.add(hub.ChannelReasoning, "igh")
	sink.add(hub.ChannelReasoning, "ing")
	select {
	case f := <-ch:
		t.Fatalf("adds inside one interval must not publish, got %+v", f)
	case <-time.After(20 * time.Millisecond):
	}

	time.Sleep(liveFlushInterval)
	sink.add(hub.ChannelReasoning, " it")

	select {
	case f := <-ch:
		if f.Live == nil {
			t.Fatalf("expected a live frame, got a committed event: %+v", f)
		}
		if f.Live.Text != "weighing it" {
			t.Fatalf("live text = %q, want the coalesced run", f.Live.Text)
		}
		if f.Live.SubTurn != 3 || f.Live.Channel != hub.ChannelReasoning {
			t.Fatalf("live frame = %+v, want sub-turn 3 on the reasoning channel", f.Live)
		}
	case <-time.After(time.Second):
		t.Fatal("no live frame after the interval elapsed")
	}

	// The tail is flushed on the way out rather than left in the builder, so
	// the last words of a response are not missing from the live view.
	sink.add(hub.ChannelContent, "the answer")
	sink.flush()
	select {
	case f := <-ch:
		if f.Live == nil || f.Live.Channel != hub.ChannelContent || f.Live.Text != "the answer" {
			t.Fatalf("expected the content tail, got %+v", f)
		}
	case <-time.After(time.Second):
		t.Fatal("flush did not publish the buffered tail")
	}
}

// A Runner with no Hub is the CLI and most tests. The sink must be a
// working no-op there rather than a nil dereference.
func TestNilLiveSinkIsANoOp(t *testing.T) {
	s := newLiveSink(nil, "sess-1", 1)
	if s != nil {
		t.Fatal("a nil Hub must produce a nil sink")
	}
	s.add(hub.ChannelReasoning, "text")
	s.flush()
}
