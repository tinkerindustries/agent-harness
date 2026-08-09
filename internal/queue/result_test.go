package queue

import (
	"encoding/json"
	"testing"
	"time"
)

func TestResultMarshalsExpectedShape(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	res := Result{
		RequestID: "req-1",
		SessionID: "sess-1",
		Status:    StatusOK,
		Result:    json.RawMessage(`{"answer":42}`),
		Text:      "done",
		Usage: &ResultUsage{
			CacheHitTokens: 100, CacheMissTokens: 5, OutputTokens: 20, ReasoningTokens: 15,
			CostUSD: 0.0001, PriceTableDate: "2026-08-09",
		},
		SubTurns:   3,
		StartedAt:  now,
		FinishedAt: now.Add(time.Minute),
	}
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var round Result
	if err := json.Unmarshal(b, &round); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if round.RequestID != res.RequestID || round.Status != res.Status || round.SubTurns != res.SubTurns {
		t.Fatalf("round-trip mismatch: %+v", round)
	}
	if round.Usage == nil || round.Usage.CacheHitTokens != 100 {
		t.Fatalf("usage did not round-trip: %+v", round.Usage)
	}
	if round.Error != nil {
		t.Fatalf("expected no error field on an ok result, got %+v", round.Error)
	}
}

func TestFinalMsgIDIsDerivedFromRequestID(t *testing.T) {
	if got, want := FinalMsgID("req-1"), "req-1.final"; got != want {
		t.Fatalf("FinalMsgID = %q, want %q", got, want)
	}
	// Two calls for the same request_id must agree, since that agreement is
	// what lets a redelivered publish deduplicate.
	if FinalMsgID("req-1") != FinalMsgID("req-1") {
		t.Fatal("FinalMsgID must be deterministic")
	}
}

func TestSubjectHelpers(t *testing.T) {
	if RequestSubject("req-1") != "harness.work.request.req-1" {
		t.Fatalf("unexpected request subject: %s", RequestSubject("req-1"))
	}
	if AcceptedSubject("req-1") != "harness.work.result.req-1.accepted" {
		t.Fatalf("unexpected accepted subject: %s", AcceptedSubject("req-1"))
	}
	if ProgressSubject("req-1") != "harness.work.result.req-1.progress" {
		t.Fatalf("unexpected progress subject: %s", ProgressSubject("req-1"))
	}
	if FinalSubject("req-1") != "harness.work.result.req-1.final" {
		t.Fatalf("unexpected final subject: %s", FinalSubject("req-1"))
	}
}
