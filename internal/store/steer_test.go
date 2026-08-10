package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
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
