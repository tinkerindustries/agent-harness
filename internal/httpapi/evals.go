package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/mrgeoffrich/deepseek-harness/internal/evals"
	"github.com/mrgeoffrich/deepseek-harness/internal/promptvariant"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

// The eval endpoints are reads over the eval_runs and eval_members tables
// (docs/EVALS.md). Every statistic is computed here rather than in the
// browser, through the same evals.Summarise the CLI's table uses, so the two
// cannot disagree about what a delta is or when it is worth looking at.

// evalRunRow is one run on the list. The counts and the headline are derived
// from its members so the list can say where a run got to without the caller
// fetching each one.
type evalRunRow struct {
	ID         string     `json:"id"`
	Suite      string     `json:"suite"`
	Note       string     `json:"note,omitempty"`
	Variants   []string   `json:"variants"`
	Replicates int        `json:"replicates"`
	JudgeModel string     `json:"judge_model,omitempty"`
	Status     string     `json:"status"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Total      int        `json:"total"`
	Finished   int        `json:"finished"`
	Failed     int        `json:"failed"`
	CostUSD    float64    `json:"cost_usd"`
	Headline   *evalDelta `json:"headline,omitempty"`
	Version    int        `json:"version"`
}

// evalMemberRow is one (task, variant, replicate).
type evalMemberRow struct {
	RequestID string         `json:"request_id"`
	TaskID    string         `json:"task_id"`
	Variant   string         `json:"variant"`
	Replicate int            `json:"replicate"`
	SessionID string         `json:"session_id,omitempty"`
	Status    string         `json:"status"`
	Scores    evals.Scores   `json:"scores,omitempty"`
	Verdict   *evals.Verdict `json:"verdict,omitempty"`
	CostUSD   float64        `json:"cost_usd"`
	SubTurns  int            `json:"sub_turns"`
	Error     string         `json:"error,omitempty"`
}

// evalDelta is one metric's comparison of the last variant against the first.
// Significant is decided here, by the same rule the CLI prints, so the browser
// reads a boolean rather than re-deriving a threshold.
type evalDelta struct {
	Metric          string  `json:"metric"`
	BaselineVariant string  `json:"baseline_variant"`
	Variant         string  `json:"variant"`
	Diff            float64 `json:"diff"`
	CombinedStderr  float64 `json:"combined_stderr"`
	Significant     bool    `json:"significant"`
}

// evalRunDetail is one run with everything needed to render it.
type evalRunDetail struct {
	evalRunRow
	Members []evalMemberRow `json:"members"`
	Summary []evals.Summary `json:"summary"`
	Deltas  []evalDelta     `json:"deltas"`
}

// evalSuiteRow is a suite this build can run.
type evalSuiteRow struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Rubric      string   `json:"rubric,omitempty"`
	TaskIDs     []string `json:"task_ids"`
}

// evalVariantRow is a prompt variant this build knows.
type evalVariantRow struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Reminder    string `json:"reminder,omitempty"`
}

// evalMembershipRow answers "which eval does this session belong to?".
type evalMembershipRow struct {
	EvalRunID string `json:"eval_run_id"`
	Suite     string `json:"suite"`
	TaskID    string `json:"task_id"`
	Variant   string `json:"variant"`
	Replicate int    `json:"replicate"`
	Status    string `json:"status"`
}

func (s *Server) handleListEvals(w http.ResponseWriter, r *http.Request) {
	runs, err := s.Store.ListEvalRuns(r.Context())
	if err != nil {
		writeInternalError(w, err)
		return
	}
	rows := make([]evalRunRow, 0, len(runs))
	for _, run := range runs {
		members, err := s.Store.EvalMembers(r.Context(), run.ID)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		rows = append(rows, evalRunRowFrom(run, members))
	}
	writeJSON(w, http.StatusOK, rows)
}

func (s *Server) handleGetEval(w http.ResponseWriter, r *http.Request) {
	run, err := s.Store.GetEvalRun(r.Context(), r.PathValue("id"))
	if err != nil {
		writeEvalLookupError(w, err)
		return
	}
	members, err := s.Store.EvalMembers(r.Context(), run.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	rows := make([]evalMemberRow, 0, len(members))
	for _, m := range members {
		rows = append(rows, evalMemberRowFrom(m))
	}
	report := evals.ReportFromStore(run, members)
	writeJSON(w, http.StatusOK, evalRunDetail{
		evalRunRow: evalRunRowFrom(run, members),
		Members:    rows,
		Summary:    evals.Summarise(report),
		Deltas:     deltasFrom(report),
	})
}

// handleGetSessionEval is the session page's lookup. A session in no eval is
// a 404, which is every ordinary session.
func (s *Server) handleGetSessionEval(w http.ResponseWriter, r *http.Request) {
	m, err := s.Store.EvalMemberForSession(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "session is not part of an eval", http.StatusNotFound)
			return
		}
		writeInternalError(w, err)
		return
	}
	run, err := s.Store.GetEvalRun(r.Context(), m.EvalRunID)
	if err != nil {
		writeEvalLookupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, evalMembershipRow{
		EvalRunID: m.EvalRunID,
		Suite:     run.Suite,
		TaskID:    m.TaskID,
		Variant:   m.Variant,
		Replicate: m.Replicate,
		Status:    m.Status,
	})
}

func (s *Server) handleListEvalSuites(w http.ResponseWriter, r *http.Request) {
	suites := evals.EmbeddedSuites()
	rows := make([]evalSuiteRow, 0, len(suites))
	for _, suite := range suites {
		ids := make([]string, 0, len(suite.Tasks))
		for _, t := range suite.Tasks {
			ids = append(ids, t.ID)
		}
		rows = append(rows, evalSuiteRow{
			Name: suite.Name, Description: suite.Description, Rubric: suite.Rubric, TaskIDs: ids,
		})
	}
	writeJSON(w, http.StatusOK, rows)
}

func (s *Server) handleListEvalVariants(w http.ResponseWriter, r *http.Request) {
	names := promptvariant.Names()
	rows := make([]evalVariantRow, 0, len(names))
	for _, name := range names {
		rows = append(rows, evalVariantRow{
			Name:        name,
			Description: promptvariant.Description(name),
			Reminder:    promptvariant.ReminderPolicyFor(name),
		})
	}
	writeJSON(w, http.StatusOK, rows)
}

func evalRunRowFrom(run store.EvalRun, members []store.EvalMember) evalRunRow {
	row := evalRunRow{
		ID: run.ID, Suite: run.Suite, Note: run.Note,
		Variants: run.Variants, Replicates: run.Replicates, JudgeModel: run.JudgeModel,
		Status: run.Status, StartedAt: run.StartedAt, FinishedAt: run.FinishedAt,
		Total: len(members), Version: run.Version,
	}
	for _, m := range members {
		row.CostUSD += m.CostUSD
		if m.Status != "pending" && m.Status != "running" {
			row.Finished++
		}
		if m.Error != "" {
			row.Failed++
		}
	}
	if deltas := deltasFrom(evals.ReportFromStore(run, members)); len(deltas) > 0 {
		row.Headline = &deltas[0]
	}
	return row
}

func evalMemberRowFrom(m store.EvalMember) evalMemberRow {
	row := evalMemberRow{
		RequestID: m.RequestID, TaskID: m.TaskID, Variant: m.Variant,
		Replicate: m.Replicate, SessionID: m.SessionID, Status: m.Status,
		CostUSD: m.CostUSD, SubTurns: m.SubTurns, Error: m.Error,
	}
	if len(m.Scores) > 0 {
		_ = json.Unmarshal(m.Scores, &row.Scores)
	}
	if len(m.Verdict) > 0 {
		var v evals.Verdict
		if json.Unmarshal(m.Verdict, &v) == nil {
			row.Verdict = &v
		}
	}
	return row
}

func deltasFrom(report *evals.Report) []evalDelta {
	out := make([]evalDelta, 0)
	for _, d := range evals.Deltas(report) {
		out = append(out, evalDelta{
			Metric: d.Metric, BaselineVariant: d.BaselineVariant, Variant: d.Variant,
			Diff: d.Diff, CombinedStderr: d.CombinedStderr, Significant: d.Significant,
		})
	}
	return out
}

// evalVersionMatches checks the If-Match header against the row. The eval
// tables carry no version-checked write inside their transactions — unlike a
// session, an eval run has one writer and no concurrent mutation to race — so
// the comparison happens here, on the row that was just read.
func evalVersionMatches(w http.ResponseWriter, r *http.Request, version int) bool {
	want, ok := parseIfMatch(w, r)
	if !ok {
		return false
	}
	if want != version {
		writeJSON(w, http.StatusPreconditionFailed, map[string]string{
			"error": fmt.Sprintf("version mismatch: the eval run is at version %d", version),
		})
		return false
	}
	return true
}

func writeEvalLookupError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "eval run not found", http.StatusNotFound)
		return
	}
	writeInternalError(w, err)
}

// --- writes ---
//
// The two POSTs are run control and carry the bearer token, the rule
// docs/RUN-CONTROL.md set for anything that starts or ends a run and spends
// money. PATCH and DELETE are data writes and carry If-Match against the
// row's version, the rule docs/DATA-API.md set for every row resource. The
// split follows the existing one exactly: an action on a run takes a token
// and no version; an edit of a row takes a version.

func (s *Server) handleStartEval(w http.ResponseWriter, r *http.Request) {
	if !writeGuards(w, r) {
		return
	}
	if !s.requireControlToken(w, r) {
		return
	}
	if s.Evals == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "evals are not configured: no orchestrator is wired (start harness serve once)",
		})
		return
	}
	var spec evals.Spec
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&spec); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid JSON body: expected an eval spec (suite, variants, replicates, ...)",
		})
		return
	}
	// Resolve here as well as in the orchestrator so a bad spec is a 400 with
	// the validator's own message rather than a 500.
	if _, err := spec.Resolve(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	id, err := s.Evals.StartEval(r.Context(), spec)
	if err != nil {
		if errors.Is(err, evals.ErrEvalInFlight) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"eval_run_id": id})
}

func (s *Server) handleCancelEval(w http.ResponseWriter, r *http.Request) {
	if !writeGuards(w, r) {
		return
	}
	if !s.requireControlToken(w, r) {
		return
	}
	if s.Evals == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "evals are not configured: no orchestrator is wired (start harness serve once)",
		})
		return
	}
	id := r.PathValue("id")
	run, err := s.Store.GetEvalRun(r.Context(), id)
	if err != nil {
		writeEvalLookupError(w, err)
		return
	}
	if run.FinishedAt != nil {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "eval run " + id + " already finished as " + run.Status,
		})
		return
	}
	if err := s.Evals.CancelEval(id); err != nil {
		// A row that says running with no run behind it is the stranded case:
		// the process that owned it died. Say so rather than pretending to
		// have cancelled something.
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "no eval run with that id is in flight in this process; close the row with PATCH if it is stranded",
		})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"eval_run_id": id, "cancelling": "true"})
}

// handlePatchEval closes out a run stranded by a dead orchestrator. It is the
// only field a caller may set: everything else about a run is what happened.
func (s *Server) handlePatchEval(w http.ResponseWriter, r *http.Request) {
	if !writeGuards(w, r) {
		return
	}
	run, err := s.Store.GetEvalRun(r.Context(), r.PathValue("id"))
	if err != nil {
		writeEvalLookupError(w, err)
		return
	}
	if !evalVersionMatches(w, r, run.Version) {
		return
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: expected {\"status\": \"...\"}"})
		return
	}
	switch body.Status {
	case store.EvalStatusOK, store.EvalStatusFailed, store.EvalStatusCancelled:
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "status must be one of ok, failed, cancelled",
		})
		return
	}
	if run.FinishedAt != nil {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "eval run already finished as " + run.Status,
		})
		return
	}
	if s.Evals != nil && s.Evals.RunningEval(run.ID) {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "eval run is still in flight here; cancel it rather than closing the row",
		})
		return
	}
	if err := s.Store.FinishEvalRun(r.Context(), run.ID, body.Status, time.Now().UTC()); err != nil {
		writeEvalLookupError(w, err)
		return
	}
	updated, err := s.Store.GetEvalRun(r.Context(), run.ID)
	if err != nil {
		writeEvalLookupError(w, err)
		return
	}
	members, err := s.Store.EvalMembers(r.Context(), run.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, evalRunRowFrom(updated, members))
}

// handleDeleteEval removes a run and its members. The sessions it names are
// ordinary sessions and are left alone.
func (s *Server) handleDeleteEval(w http.ResponseWriter, r *http.Request) {
	if !writeGuards(w, r) {
		return
	}
	run, err := s.Store.GetEvalRun(r.Context(), r.PathValue("id"))
	if err != nil {
		writeEvalLookupError(w, err)
		return
	}
	if !evalVersionMatches(w, r, run.Version) {
		return
	}
	if run.FinishedAt == nil {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "eval run is still running; cancel it before deleting",
		})
		return
	}
	if err := s.Store.DeleteEvalRun(r.Context(), run.ID); err != nil {
		writeEvalLookupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
