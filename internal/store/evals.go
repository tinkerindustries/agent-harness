package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Eval run statuses. A run is Running until every member reaches a terminal
// state; Cancelled is what a partial run that was stopped early records, and
// its comparison is a legitimate result over the members that did finish.
const (
	EvalStatusRunning   = "running"
	EvalStatusOK        = "ok"
	EvalStatusFailed    = "failed"
	EvalStatusCancelled = "cancelled"
)

// EvalRun is one eval run: the suite, the arms compared, and how it ended.
// The rows it owns are in eval_members.
type EvalRun struct {
	ID         string
	Suite      string
	SuiteJSON  json.RawMessage
	Variants   []string
	Replicates int
	JudgeModel string
	Note       string
	Status     string
	StartedAt  time.Time
	FinishedAt *time.Time
	Version    int
}

// EvalMember is one (task, variant, replicate) and what came of it. Scores
// and Verdict are the JSON the evals package wrote; this package does not
// interpret either, which keeps the metric vocabulary out of the schema.
type EvalMember struct {
	EvalRunID string
	RequestID string
	TaskID    string
	Variant   string
	Replicate int
	SessionID string
	Status    string
	Scores    json.RawMessage
	Verdict   json.RawMessage
	CostUSD   float64
	SubTurns  int
	Error     string
}

const evalRunColumns = `id, suite, suite_json, variants, replicates, judge_model, note, status, started_at, finished_at, version`

const evalMemberColumns = `eval_run_id, request_id, task_id, variant, replicate, session_id, status, scores, verdict, cost_usd, sub_turns, error`

// CreateEvalRun records a run and every member it intends to publish, in one
// transaction. The members exist before anything is published so a run that
// dies mid-flight still says what it was going to do.
func (s *Store) CreateEvalRun(ctx context.Context, run EvalRun, members []EvalMember) error {
	variants, err := json.Marshal(run.Variants)
	if err != nil {
		return fmt.Errorf("store: encode eval variants: %w", err)
	}
	return s.submit(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(
			`INSERT INTO eval_runs (`+evalRunColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)`,
			run.ID, run.Suite, string(run.SuiteJSON), string(variants), run.Replicates,
			run.JudgeModel, run.Note, run.Status, formatTime(run.StartedAt), nullTime(run.FinishedAt),
		); err != nil {
			return err
		}
		for _, m := range members {
			if _, err := tx.Exec(
				`INSERT INTO eval_members (`+evalMemberColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				m.EvalRunID, m.RequestID, m.TaskID, m.Variant, m.Replicate,
				nullString(m.SessionID), m.Status, nullJSON(m.Scores), nullJSON(m.Verdict),
				m.CostUSD, m.SubTurns, m.Error,
			); err != nil {
				return err
			}
		}
		return nil
	})
}

// UpdateEvalMember writes one member's outcome. It is called as each run
// finishes, so a report exists for the part of an eval that has completed
// even while the rest is still going.
func (s *Store) UpdateEvalMember(ctx context.Context, m EvalMember) error {
	return s.submit(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			UPDATE eval_members
			SET session_id = ?, status = ?, scores = ?, verdict = ?, cost_usd = ?, sub_turns = ?, error = ?
			WHERE eval_run_id = ? AND request_id = ?`,
			nullString(m.SessionID), m.Status, nullJSON(m.Scores), nullJSON(m.Verdict),
			m.CostUSD, m.SubTurns, m.Error, m.EvalRunID, m.RequestID)
		return err
	})
}

// FinishEvalRun closes a run out.
func (s *Store) FinishEvalRun(ctx context.Context, id, status string, finishedAt time.Time) error {
	return s.submit(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(
			`UPDATE eval_runs SET status = ?, finished_at = ?, version = version + 1 WHERE id = ?`,
			status, formatTime(finishedAt), id)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// GetEvalRun returns one run without its members.
func (s *Store) GetEvalRun(ctx context.Context, id string) (EvalRun, error) {
	row := s.readDB.QueryRowContext(ctx, `SELECT `+evalRunColumns+` FROM eval_runs WHERE id = ?`, id)
	run, err := scanEvalRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return EvalRun{}, ErrNotFound
	}
	return run, err
}

// ListEvalRuns returns every run, newest first by started_at — the same
// convention ListSessions and ListWorkRequests follow.
func (s *Store) ListEvalRuns(ctx context.Context) ([]EvalRun, error) {
	rows, err := s.readDB.QueryContext(ctx,
		`SELECT `+evalRunColumns+` FROM eval_runs ORDER BY started_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs := []EvalRun{}
	for rows.Next() {
		run, err := scanEvalRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

// EvalMembers returns a run's members in a stable order: by task as the suite
// declares them is not knowable here, so by task id, then variant, then
// replicate, which groups the arms of one task together.
func (s *Store) EvalMembers(ctx context.Context, evalRunID string) ([]EvalMember, error) {
	rows, err := s.readDB.QueryContext(ctx,
		`SELECT `+evalMemberColumns+` FROM eval_members WHERE eval_run_id = ?
		 ORDER BY task_id, variant, replicate`, evalRunID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	members := []EvalMember{}
	for rows.Next() {
		m, err := scanEvalMember(rows)
		if err != nil {
			return nil, err
		}
		members = append(members, m)
	}
	return members, rows.Err()
}

// EvalMemberForSession answers the session page's question: which eval does
// this session belong to? ErrNotFound when it belongs to none, which is every
// ordinary session.
func (s *Store) EvalMemberForSession(ctx context.Context, sessionID string) (EvalMember, error) {
	row := s.readDB.QueryRowContext(ctx,
		`SELECT `+evalMemberColumns+` FROM eval_members WHERE session_id = ?`, sessionID)
	m, err := scanEvalMember(row)
	if errors.Is(err, sql.ErrNoRows) {
		return EvalMember{}, ErrNotFound
	}
	return m, err
}

// DeleteEvalRun removes a run and its members. The sessions it names are left
// alone: they are ordinary sessions and have their own delete.
func (s *Store) DeleteEvalRun(ctx context.Context, id string) error {
	return s.submit(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM eval_members WHERE eval_run_id = ?`, id); err != nil {
			return err
		}
		res, err := tx.Exec(`DELETE FROM eval_runs WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanEvalRun(sc rowScanner) (EvalRun, error) {
	var (
		run        EvalRun
		suiteJSON  string
		variants   string
		startedAt  string
		finishedAt sql.NullString
	)
	if err := sc.Scan(&run.ID, &run.Suite, &suiteJSON, &variants, &run.Replicates,
		&run.JudgeModel, &run.Note, &run.Status, &startedAt, &finishedAt, &run.Version); err != nil {
		return EvalRun{}, err
	}
	run.SuiteJSON = json.RawMessage(suiteJSON)
	if err := json.Unmarshal([]byte(variants), &run.Variants); err != nil {
		return EvalRun{}, fmt.Errorf("store: decode eval variants for %s: %w", run.ID, err)
	}
	t, err := parseTime(startedAt)
	if err != nil {
		return EvalRun{}, err
	}
	run.StartedAt = t
	if finishedAt.Valid {
		f, err := parseTime(finishedAt.String)
		if err != nil {
			return EvalRun{}, err
		}
		run.FinishedAt = &f
	}
	return run, nil
}

func scanEvalMember(sc rowScanner) (EvalMember, error) {
	var (
		m         EvalMember
		sessionID sql.NullString
		scores    sql.NullString
		verdict   sql.NullString
	)
	if err := sc.Scan(&m.EvalRunID, &m.RequestID, &m.TaskID, &m.Variant, &m.Replicate,
		&sessionID, &m.Status, &scores, &verdict, &m.CostUSD, &m.SubTurns, &m.Error); err != nil {
		return EvalMember{}, err
	}
	m.SessionID = sessionID.String
	if scores.Valid {
		m.Scores = json.RawMessage(scores.String)
	}
	if verdict.Valid {
		m.Verdict = json.RawMessage(verdict.String)
	}
	return m, nil
}

// The store writes RFC3339 and reads RFC3339Nano, which is what every other
// table here does.
func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTime(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}

func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return formatTime(*t)
}
