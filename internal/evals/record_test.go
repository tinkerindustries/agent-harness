package evals

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mrgeoffrich/deepseek-harness/internal/queue"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

// fakeQueue stands in for the WORK stream and the workers behind it: it
// records what was published and immediately finishes each request with a
// session that made one Grep call, so Execute's recording path can be driven
// without a broker or an API.
type fakeQueue struct {
	mu        sync.Mutex
	published []queue.Request
	st        *store.Store
	// finishStatus is what the work request lands as. Empty is "ok".
	finishStatus string
}

func (q *fakeQueue) Publish(ctx context.Context, req queue.Request) error {
	q.mu.Lock()
	q.published = append(q.published, req)
	q.mu.Unlock()

	sessionID := "sess-" + req.RequestID
	if err := q.st.CreateSession(ctx, store.Session{
		ID: sessionID, Model: "deepseek-v4-flash", Effort: "high",
		Workspace: "/ws", PermissionMode: "readonly",
		SystemPrompt: "p", ToolSchema: json.RawMessage(`[]`), Status: store.StatusRunning,
	}); err != nil {
		return err
	}
	callPayload, _ := json.Marshal(store.ToolCallPayload{ID: "c1", Name: "Grep", Arguments: `{"pattern":"x"}`})
	resultPayload, _ := json.Marshal(store.ToolResultPayload{ToolCallID: "c1", Name: "Grep", Content: "hit"})
	if _, err := q.st.AppendEvents(ctx, sessionID, []store.EventInput{
		{Kind: store.KindToolCall, Payload: json.RawMessage(callPayload)},
		{Kind: store.KindToolResult, Payload: json.RawMessage(resultPayload)},
		{Kind: store.KindUsage, Payload: store.UsagePayload{SubTurn: 3, PromptTokens: 9000, CostUSD: 0.25}},
	}); err != nil {
		return err
	}
	if _, err := q.st.ClaimWorkRequest(ctx, req.RequestID, 1, time.Now().UTC()); err != nil {
		return err
	}
	if err := q.st.SetWorkRequestSession(ctx, req.RequestID, sessionID); err != nil {
		return err
	}
	status := q.finishStatus
	if status == "" {
		status = "ok"
	}
	_, err := q.st.FinishWorkRequest(ctx, req.RequestID, sessionID, status, nil, time.Now().UTC())
	return err
}

func testSuite() *Suite {
	return &Suite{
		Name:   "t",
		Rubric: "r",
		Tasks: []Task{{
			ID: "task-a", Prompt: "do it",
			Repos: []queue.Repo{{URL: "https://example.com/r"}},
		}},
	}
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "harness.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestExecuteRecordsTheRunAndItsMembers(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	q := &fakeQueue{st: st}

	report, err := Execute(ctx, q, st, Options{
		Suite:        testSuite(),
		Variants:     []string{"base", "search-first"},
		Replicates:   2,
		Concurrency:  2,
		PollInterval: time.Millisecond,
		Timeout:      10 * time.Second,
		Recorder:     st,
		Note:         "a note",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if report.EvalRunID == "" {
		t.Fatal("report carries no eval run id")
	}
	if len(q.published) != 4 {
		t.Fatalf("published %d requests, want 4", len(q.published))
	}

	run, err := st.GetEvalRun(ctx, report.EvalRunID)
	if err != nil {
		t.Fatalf("GetEvalRun: %v", err)
	}
	if run.Status != store.EvalStatusOK {
		t.Errorf("status = %q, want %q", run.Status, store.EvalStatusOK)
	}
	if run.FinishedAt == nil {
		t.Error("run was not closed out")
	}
	if run.Note != "a note" || run.Suite != "t" {
		t.Errorf("run = %+v", run)
	}

	members, err := st.EvalMembers(ctx, report.EvalRunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 4 {
		t.Fatalf("members = %d, want 4", len(members))
	}
	for _, m := range members {
		if m.Status != "ok" {
			t.Errorf("member %s status = %q", m.RequestID, m.Status)
		}
		if m.SessionID == "" {
			t.Errorf("member %s has no session", m.RequestID)
		}
		var scores Scores
		if err := json.Unmarshal(m.Scores, &scores); err != nil {
			t.Fatalf("member %s scores: %v", m.RequestID, err)
		}
		if scores["search_via_tool"] != 1 {
			t.Errorf("member %s search_via_tool = %v, want 1", m.RequestID, scores["search_via_tool"])
		}
		if m.CostUSD != 0.25 || m.SubTurns != 3 {
			t.Errorf("member %s cost = %v, sub_turns = %d", m.RequestID, m.CostUSD, m.SubTurns)
		}
	}
}

// The members are written before anything is published, so a run that dies
// mid-flight still says what it was going to do.
func TestExecuteRecordsMembersBeforePublishing(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()

	var seen int
	q := &publishSpy{onPublish: func() {
		runs, err := st.ListEvalRuns(ctx)
		if err != nil || len(runs) != 1 {
			t.Errorf("no eval run recorded before the first publish: %v", err)
			return
		}
		members, err := st.EvalMembers(ctx, runs[0].ID)
		if err != nil {
			t.Error(err)
			return
		}
		seen = len(members)
	}}

	if _, err := Execute(ctx, q, st, Options{
		Suite: testSuite(), Variants: []string{"base", "search-first"},
		Replicates: 1, Concurrency: 1, PollInterval: time.Millisecond,
		Timeout: 200 * time.Millisecond, Recorder: st,
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if seen != 2 {
		t.Errorf("saw %d members at the first publish, want 2", seen)
	}
}

// A run whose members all failed is a failed run. One where some finished is
// an ok run with failed members: the comparison over what did finish stands.
func TestExecuteRecordsAFailedRunWhenNothingFinished(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()

	report, err := Execute(ctx, &publishSpy{}, st, Options{
		Suite: testSuite(), Variants: []string{"base", "search-first"},
		Replicates: 1, Concurrency: 2, PollInterval: time.Millisecond,
		Timeout: 100 * time.Millisecond, Recorder: st,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	run, err := st.GetEvalRun(ctx, report.EvalRunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.EvalStatusFailed {
		t.Errorf("status = %q, want %q", run.Status, store.EvalStatusFailed)
	}
	members, err := st.EvalMembers(ctx, report.EvalRunID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range members {
		if m.Error == "" {
			t.Errorf("member %s records no error", m.RequestID)
		}
	}
}

// publishSpy accepts a publish and never finishes the request, so the run
// times out.
type publishSpy struct {
	mu        sync.Mutex
	onPublish func()
}

func (p *publishSpy) Publish(ctx context.Context, req queue.Request) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.onPublish != nil {
		p.onPublish()
		p.onPublish = nil
	}
	return nil
}

// Nothing is written when no recorder is attached: the report is then the
// only output, which is what a caller with no store gets.
func TestExecuteWithoutARecorderWritesNothing(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()

	if _, err := Execute(ctx, &publishSpy{}, st, Options{
		Suite: testSuite(), Variants: []string{"base", "search-first"},
		Replicates: 1, Concurrency: 2, PollInterval: time.Millisecond,
		Timeout: 50 * time.Millisecond,
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	runs, err := st.ListEvalRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Errorf("runs = %d, want 0 without a recorder", len(runs))
	}
}

// A member that timed out carries a status and no error string. The run is
// not a success just because nothing raised an error.
func TestExecuteRecordsAFailedRunWhenEveryMemberTimedOut(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	q := &fakeQueue{st: st, finishStatus: "timeout"}

	report, err := Execute(ctx, q, st, Options{
		Suite: testSuite(), Variants: []string{"base", "search-first"},
		Replicates: 1, Concurrency: 2, PollInterval: time.Millisecond,
		Timeout: 10 * time.Second, Recorder: st,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	run, err := st.GetEvalRun(ctx, report.EvalRunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.EvalStatusFailed {
		t.Errorf("status = %q, want %q — every member timed out", run.Status, store.EvalStatusFailed)
	}
}
