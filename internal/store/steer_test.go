package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestSteerMessagesAfter pins the session loop's per-sub-turn steer query:
// only steer_message events, in seq order, past the given seq and capped by
// the limit, and — following RequestStatus's rule — nothing but steer_message
// payloads ever read off the disk.
func TestSteerMessagesAfter(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateSession(t, s, "sess-1")

	if _, err := s.AppendEvents(ctx, "sess-1", []EventInput{
		{Kind: KindTurnStarted, Payload: TurnStartedPayload{SubTurn: 1}},
		{Kind: KindSteerMessage, Payload: SteerMessagePayload{Text: "first", Source: "web"}},
		{Kind: KindContentDelta, Payload: ContentDeltaPayload{Text: strings.Repeat("x", 1<<20)}},
		{Kind: KindSteerApplied, Payload: SteerAppliedPayload{SourceSeq: 2, Text: "first", SubTurn: 1}},
		{Kind: KindSteerMessage, Payload: SteerMessagePayload{Text: "second", Source: "cli"}},
		{Kind: KindSteerMessage, Payload: SteerMessagePayload{Text: "third", Source: "mcp"}},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.SteerMessagesAfter(ctx, "sess-1", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 steer_message events, got %d", len(got))
	}
	for i, want := range []string{"first", "second", "third"} {
		if got[i].Kind != KindSteerMessage {
			t.Fatalf("event %d kind = %s, want steer_message only", i, got[i].Kind)
		}
		var p SteerMessagePayload
		if err := json.Unmarshal(got[i].Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.Text != want {
			t.Fatalf("event %d text = %q, want %q", i, p.Text, want)
		}
	}

	// afterSeq filters, still in seq order.
	got, err = s.SteerMessagesAfter(ctx, "sess-1", 2, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Seq != 5 || got[1].Seq != 6 {
		t.Fatalf("afterSeq=2: expected seqs 5,6, got %+v", got)
	}

	// The limit caps the batch.
	got, err = s.SteerMessagesAfter(ctx, "sess-1", 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Seq != 2 || got[1].Seq != 5 {
		t.Fatalf("limit=2: expected seqs 2,5, got %+v", got)
	}
}

// TestLastAppliedSteerSeq pins the high-water mark query: 0 with no
// steer_applied events, and the highest SourceSeq otherwise.
func TestLastAppliedSteerSeq(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateSession(t, s, "sess-1")

	max, err := s.LastAppliedSteerSeq(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if max != 0 {
		t.Fatalf("no steer_applied events: expected 0, got %d", max)
	}

	if _, err := s.AppendEvents(ctx, "sess-1", []EventInput{
		{Kind: KindSteerApplied, Payload: SteerAppliedPayload{SourceSeq: 2, Text: "a", SubTurn: 1}},
		{Kind: KindSteerApplied, Payload: SteerAppliedPayload{SourceSeq: 9, Text: "b", SubTurn: 2}},
		{Kind: KindSteerApplied, Payload: SteerAppliedPayload{SourceSeq: 4, Text: "c", SubTurn: 3}},
	}); err != nil {
		t.Fatal(err)
	}
	max, err = s.LastAppliedSteerSeq(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if max != 9 {
		t.Fatalf("expected the highest SourceSeq 9, got %d", max)
	}
}

// TestWithdrawUnappliedSteers pins the sweep a host runs when a run ends:
// every steer_message past the applied mark is closed with a steer_withdrawn
// naming it, in seq order, and the mark then counts those steers as consumed,
// so the next run's SteerMessagesAfter(mark) finds nothing. An applied steer
// is left alone, and a second sweep withdraws nothing. It also writes to a
// cancelled session, which AppendEvents refuses, because a cancelled run is
// the one likeliest to leave a steer behind.
func TestWithdrawUnappliedSteers(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateSession(t, s, "sess-1")

	if _, err := s.AppendEvents(ctx, "sess-1", []EventInput{
		{Kind: KindSteerMessage, Payload: SteerMessagePayload{Text: "applied"}},
		{Kind: KindSteerApplied, Payload: SteerAppliedPayload{SourceSeq: 1, Text: "applied", SubTurn: 1}},
		{Kind: KindSteerMessage, Payload: SteerMessagePayload{Text: "late one"}},
		{Kind: KindRunFinished, Payload: RunFinishedPayload{Reason: "no_tool_calls"}},
		{Kind: KindSteerMessage, Payload: SteerMessagePayload{Text: "late two"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.CancelRunningSession(ctx, "sess-1", time.Now().UTC()); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	got, err := s.WithdrawUnappliedSteers(ctx, "sess-1")
	if err != nil {
		t.Fatalf("withdraw on a cancelled session: %v", err)
	}
	if len(got) != 2 || got[0] != 3 || got[1] != 5 {
		t.Fatalf("withdrew %v, want [3 5]", got)
	}

	events, err := s.GetEvents(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	var sources []int64
	for _, e := range events {
		if e.Kind != KindSteerWithdrawn {
			continue
		}
		var p SteerWithdrawnPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		sources = append(sources, p.SourceSeq)
	}
	if len(sources) != 2 || sources[0] != 3 || sources[1] != 5 {
		t.Fatalf("steer_withdrawn source seqs = %v, want [3 5]", sources)
	}
	if last := events[len(events)-1].Seq; last != 7 {
		t.Errorf("last seq = %d, want 7: the withdrawals follow the log's tail", last)
	}

	mark, err := s.LastAppliedSteerSeq(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if mark != 5 {
		t.Fatalf("mark after withdrawal = %d, want 5", mark)
	}
	pending, err := s.SteerMessagesAfter(ctx, "sess-1", mark, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("%d steers still pending past the mark after withdrawal", len(pending))
	}

	again, err := s.WithdrawUnappliedSteers(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Errorf("a second sweep withdrew %v", again)
	}
}
