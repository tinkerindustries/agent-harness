package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func seedEvalRun(t *testing.T, s *Store, id string, members []EvalMember) EvalRun {
	t.Helper()
	run := EvalRun{
		ID:         id,
		Suite:      "search",
		SuiteJSON:  json.RawMessage(`{"name":"search"}`),
		Variants:   []string{"base", "search-first"},
		Replicates: 2,
		JudgeModel: "deepseek-v4-pro",
		Note:       "does naming the tools help",
		Status:     EvalStatusRunning,
		StartedAt:  time.Date(2026, 8, 13, 4, 0, 0, 0, time.UTC),
	}
	if err := s.CreateEvalRun(context.Background(), run, members); err != nil {
		t.Fatalf("CreateEvalRun: %v", err)
	}
	return run
}

func member(runID, requestID, variant string) EvalMember {
	return EvalMember{
		EvalRunID: runID, RequestID: requestID,
		TaskID: "find-cache-invariants", Variant: variant, Replicate: 1,
		Status: "pending",
	}
}

func TestCreateAndGetEvalRun(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	want := seedEvalRun(t, s, "evr-1", []EvalMember{member("evr-1", "eval-a", "base")})

	got, err := s.GetEvalRun(ctx, "evr-1")
	if err != nil {
		t.Fatalf("GetEvalRun: %v", err)
	}
	if got.Suite != want.Suite || got.Note != want.Note || got.JudgeModel != want.JudgeModel {
		t.Errorf("run = %+v, want %+v", got, want)
	}
	if len(got.Variants) != 2 || got.Variants[0] != "base" {
		t.Errorf("variants = %v", got.Variants)
	}
	if !got.StartedAt.Equal(want.StartedAt) {
		t.Errorf("started_at = %v, want %v", got.StartedAt, want.StartedAt)
	}
	if got.FinishedAt != nil {
		t.Errorf("finished_at = %v, want nil on a running run", got.FinishedAt)
	}
	if got.Version != 1 {
		t.Errorf("version = %d, want 1", got.Version)
	}
}

// The suite is stored verbatim: evals/search.json changes, and a run has to
// keep saying what it actually ran.
func TestEvalRunKeepsTheSuiteItRan(t *testing.T) {
	s := openTestStore(t)
	seedEvalRun(t, s, "evr-1", nil)
	got, err := s.GetEvalRun(context.Background(), "evr-1")
	if err != nil {
		t.Fatal(err)
	}
	if string(got.SuiteJSON) != `{"name":"search"}` {
		t.Errorf("suite_json = %s", got.SuiteJSON)
	}
}

func TestGetEvalRunNotFound(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.GetEvalRun(context.Background(), "evr-missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestUpdateEvalMemberWritesTheOutcome(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedEvalRun(t, s, "evr-1", []EvalMember{member("evr-1", "eval-a", "base")})

	done := member("evr-1", "eval-a", "base")
	done.SessionID = "sess-1"
	done.Status = "ok"
	done.Scores = json.RawMessage(`{"search_via_tool":0.75}`)
	done.Verdict = json.RawMessage(`{"score":4,"completed":true}`)
	done.CostUSD = 0.0812
	done.SubTurns = 31
	if err := s.UpdateEvalMember(ctx, done); err != nil {
		t.Fatalf("UpdateEvalMember: %v", err)
	}

	members, err := s.EvalMembers(ctx, "evr-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 {
		t.Fatalf("members = %d, want 1", len(members))
	}
	got := members[0]
	if got.Status != "ok" || got.SessionID != "sess-1" || got.SubTurns != 31 {
		t.Errorf("member = %+v", got)
	}
	if string(got.Scores) != `{"search_via_tool":0.75}` {
		t.Errorf("scores = %s", got.Scores)
	}
	if string(got.Verdict) != `{"score":4,"completed":true}` {
		t.Errorf("verdict = %s", got.Verdict)
	}
}

// A member with no verdict — no -judge, or a judge call that failed — reads
// back as absent rather than as an empty object that would parse as score 0.
func TestEvalMemberWithoutAVerdictReadsBackNil(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedEvalRun(t, s, "evr-1", []EvalMember{member("evr-1", "eval-a", "base")})

	members, err := s.EvalMembers(ctx, "evr-1")
	if err != nil {
		t.Fatal(err)
	}
	if members[0].Verdict != nil || members[0].Scores != nil {
		t.Errorf("verdict = %v, scores = %v, want both nil", members[0].Verdict, members[0].Scores)
	}
	if members[0].SessionID != "" {
		t.Errorf("session_id = %q, want empty before the session exists", members[0].SessionID)
	}
}

func TestEvalMembersGroupsTheArmsOfOneTask(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	a := member("evr-1", "eval-a", "search-first")
	b := member("evr-1", "eval-b", "base")
	c := member("evr-1", "eval-c", "base")
	c.TaskID = "trace-permission-mode"
	seedEvalRun(t, s, "evr-1", []EvalMember{a, b, c})

	members, err := s.EvalMembers(ctx, "evr-1")
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, m := range members {
		order = append(order, m.TaskID+"/"+m.Variant)
	}
	want := []string{
		"find-cache-invariants/base",
		"find-cache-invariants/search-first",
		"trace-permission-mode/base",
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

func TestEvalMemberForSession(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	m := member("evr-1", "eval-a", "base")
	seedEvalRun(t, s, "evr-1", []EvalMember{m})
	m.SessionID = "sess-1"
	m.Status = "ok"
	if err := s.UpdateEvalMember(ctx, m); err != nil {
		t.Fatal(err)
	}

	got, err := s.EvalMemberForSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("EvalMemberForSession: %v", err)
	}
	if got.EvalRunID != "evr-1" || got.Variant != "base" {
		t.Errorf("member = %+v", got)
	}
	if _, err := s.EvalMemberForSession(ctx, "sess-ordinary"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound for a session in no eval", err)
	}
}

func TestFinishEvalRun(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedEvalRun(t, s, "evr-1", nil)
	finishedAt := time.Date(2026, 8, 13, 5, 0, 0, 0, time.UTC)
	if err := s.FinishEvalRun(ctx, "evr-1", EvalStatusOK, finishedAt); err != nil {
		t.Fatalf("FinishEvalRun: %v", err)
	}

	got, err := s.GetEvalRun(ctx, "evr-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != EvalStatusOK {
		t.Errorf("status = %q, want %q", got.Status, EvalStatusOK)
	}
	if got.FinishedAt == nil || !got.FinishedAt.Equal(finishedAt) {
		t.Errorf("finished_at = %v, want %v", got.FinishedAt, finishedAt)
	}
	if got.Version != 2 {
		t.Errorf("version = %d, want 2 after one mutation", got.Version)
	}
	if err := s.FinishEvalRun(ctx, "evr-missing", EvalStatusOK, finishedAt); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestListEvalRunsNewestFirst(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	for i, id := range []string{"evr-old", "evr-new"} {
		run := EvalRun{
			ID: id, Suite: "search", SuiteJSON: json.RawMessage(`{}`),
			Variants: []string{"base", "search-first"}, Replicates: 1,
			Status:    EvalStatusOK,
			StartedAt: time.Date(2026, 8, 13, i, 0, 0, 0, time.UTC),
		}
		if err := s.CreateEvalRun(ctx, run, nil); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := s.ListEvalRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].ID != "evr-new" {
		t.Errorf("runs = %v, want evr-new first", runs)
	}
}

// Deleting a run takes its members and leaves the sessions, which are
// ordinary sessions with their own delete.
func TestDeleteEvalRunRemovesItsMembers(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedEvalRun(t, s, "evr-1", []EvalMember{member("evr-1", "eval-a", "base")})

	if err := s.DeleteEvalRun(ctx, "evr-1"); err != nil {
		t.Fatalf("DeleteEvalRun: %v", err)
	}
	if _, err := s.GetEvalRun(ctx, "evr-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("run still present: %v", err)
	}
	members, err := s.EvalMembers(ctx, "evr-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 0 {
		t.Errorf("members = %d, want 0", len(members))
	}
	if err := s.DeleteEvalRun(ctx, "evr-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound on a second delete", err)
	}
}

// The tables are created by the schema constant rather than a migration, so
// an older database gains them on the next Open with no backfill.
func TestEvalTablesAppearOnAnExistingDatabase(t *testing.T) {
	s := openTestStore(t)
	var name string
	err := s.readDB.QueryRow(
		`SELECT name FROM sqlite_master WHERE type = 'index' AND name = 'idx_eval_members_session'`).Scan(&name)
	if err != nil {
		t.Fatalf("the session lookup index is missing: %v", err)
	}
}
