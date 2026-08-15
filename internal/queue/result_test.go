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

// TestSubjectHelpers pins the production subject contract of
// docs/DESIGN.md §4.10 — the one subject that still exists. TestMain has by
// then renamed the package's subject prefix via IsolateForTest so the broker
// tests do not share subjects with other packages, so this unit test
// restores the production prefix for its own assertion; the helper's job is
// to build a subject, and the contract it pins is the production one.
func TestSubjectHelpers(t *testing.T) {
	origReq := requestSubjectPrefix
	requestSubjectPrefix = "harness.work.request."
	defer func() { requestSubjectPrefix = origReq }()

	if RequestSubject("req-1") != "harness.work.request.req-1" {
		t.Fatalf("unexpected request subject: %s", RequestSubject("req-1"))
	}
}
