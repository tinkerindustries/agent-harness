package evals

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/mrgeoffrich/deepseek-harness/internal/queue"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

// A Run is one (task, variant, replicate) triple and what came of it.
type Run struct {
	TaskID    string   `json:"task_id"`
	Variant   string   `json:"variant"`
	Replicate int      `json:"replicate"`
	RequestID string   `json:"request_id"`
	SessionID string   `json:"session_id,omitempty"`
	Status    string   `json:"status,omitempty"`
	Scores    Scores   `json:"scores,omitempty"`
	Verdict   *Verdict `json:"verdict,omitempty"`
	CostUSD   float64  `json:"cost_usd,omitempty"`
	SubTurns  int      `json:"sub_turns,omitempty"`
	Err       string   `json:"error,omitempty"`
}

// A Report is everything one `harness eval run` produced. It is an export of
// the eval_runs and eval_members rows rather than the record itself: the rows
// are written as the run goes and outlive the process.
type Report struct {
	EvalRunID  string    `json:"eval_run_id"`
	Suite      string    `json:"suite"`
	Variants   []string  `json:"variants"`
	Replicates int       `json:"replicates"`
	StartedAt  time.Time `json:"started_at"`
	Runs       []Run     `json:"runs"`
}

// Sessions is the slice of the store an eval run reads. Everything scored
// comes from stored events, so an eval is a reader of the same data the UI
// shows and a report can be re-scored later without touching the model.
type Sessions interface {
	GetWorkRequest(ctx context.Context, requestID string) (store.WorkRequest, error)
	GetEvents(ctx context.Context, sessionID string) ([]store.Event, error)
}

// Recorder is where a run's progress is written as it happens, so the eval
// survives the terminal it was started from and a browser can read a run that
// is still going. Nil records nothing and the report is the only output.
type Recorder interface {
	CreateEvalRun(ctx context.Context, run store.EvalRun, members []store.EvalMember) error
	UpdateEvalMember(ctx context.Context, m store.EvalMember) error
	FinishEvalRun(ctx context.Context, id, status string, finishedAt time.Time) error
}

// Publisher publishes one validated work request, the same seam the HTTP
// server's run-control uses.
type Publisher interface {
	Publish(ctx context.Context, req queue.Request) error
}

// Options configure one eval run.
type Options struct {
	Suite      *Suite
	Variants   []string
	Replicates int
	// Concurrency bounds how many runs are in flight at once. The worker pool
	// has its own limit; this one keeps an eval from filling every slot and
	// starving whatever else the stack is doing.
	Concurrency int
	// PollInterval is how often a pending run's session is checked. Runs take
	// minutes, so this is seconds rather than milliseconds.
	PollInterval time.Duration
	// Timeout bounds one run end to end.
	Timeout time.Duration
	// MaxSubTurns, when set, overrides every task's own budget. It applies to
	// every arm at once, which is the only safe way to give a run more room:
	// extending the arm that keeps running out would hand extra budget to
	// whichever variant is less efficient, hiding the difference the eval
	// exists to measure.
	MaxSubTurns int
	// Model and Effort override every task's, for the same reason and with
	// the same rule: they apply to all arms at once. A comparison across two
	// models is not a comparison of two prompts.
	Model  string
	Effort string
	// Judge, when set, scores each finished transcript.
	Judge *Judge
	// Recorder, when set, is where the run and its members are written as
	// they happen.
	Recorder Recorder
	// Note is the operator's one line on what this run is asking.
	Note string
	// EvalRunID names the run. Empty mints one.
	EvalRunID string
	// StartedAt stamps the run. Zero is time.Now.
	StartedAt time.Time
	// Progress, when set, is called as each run finishes.
	Progress func(Run)
}

// Execute publishes every run, waits for each to finish, and scores it.
// A run that fails is reported with its error rather than dropped, because a
// variant that crashes runs is a result about that variant.
func Execute(ctx context.Context, pub Publisher, sessions Sessions, opts Options) (*Report, error) {
	if err := ValidateVariants(opts.Variants); err != nil {
		return nil, err
	}
	if opts.Replicates < 1 {
		return nil, fmt.Errorf("evals: replicates must be at least 1")
	}
	if opts.Concurrency < 1 {
		opts.Concurrency = 2
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 5 * time.Second
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Minute
	}

	evalRunID := opts.EvalRunID
	if evalRunID == "" {
		evalRunID = newID("evr")
	}
	startedAt := opts.StartedAt
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	report := &Report{
		EvalRunID:  evalRunID,
		Suite:      opts.Suite.Name,
		Variants:   opts.Variants,
		Replicates: opts.Replicates,
		StartedAt:  startedAt,
	}

	// The work list interleaves variants rather than running one arm and then
	// the other, so a change in the machine or the API partway through hits
	// both arms alike instead of landing entirely on one.
	var pending []Run
	for rep := 1; rep <= opts.Replicates; rep++ {
		for _, task := range opts.Suite.Tasks {
			for _, variant := range opts.Variants {
				pending = append(pending, Run{
					TaskID:    task.ID,
					Variant:   variant,
					Replicate: rep,
					RequestID: newID("eval"),
				})
			}
		}
	}

	tasksByID := map[string]Task{}
	for _, t := range opts.Suite.Tasks {
		tasksByID[t.ID] = t
	}

	// The members exist before anything is published, so a run that dies
	// mid-flight still says what it was going to do.
	if opts.Recorder != nil {
		members := make([]store.EvalMember, 0, len(pending))
		for _, r := range pending {
			members = append(members, storeMember(evalRunID, r, "pending"))
		}
		judgeModel := ""
		if opts.Judge != nil {
			judgeModel = opts.Judge.Model
		}
		suiteJSON, err := json.Marshal(opts.Suite)
		if err != nil {
			return nil, fmt.Errorf("evals: encode suite: %w", err)
		}
		if err := opts.Recorder.CreateEvalRun(ctx, store.EvalRun{
			ID:         evalRunID,
			Suite:      opts.Suite.Name,
			SuiteJSON:  suiteJSON,
			Variants:   opts.Variants,
			Replicates: opts.Replicates,
			JudgeModel: judgeModel,
			Note:       opts.Note,
			Status:     store.EvalStatusRunning,
			StartedAt:  startedAt,
		}, members); err != nil {
			return nil, fmt.Errorf("evals: record eval run: %w", err)
		}
	}

	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		sem  = make(chan struct{}, opts.Concurrency)
		done = make([]Run, len(pending))
	)
	for i, r := range pending {
		wg.Add(1)
		go func(i int, r Run) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				r.Err = ctx.Err().Error()
				done[i] = r
				return
			}
			defer func() { <-sem }()

			execute(ctx, pub, sessions, opts, tasksByID[r.TaskID], &r)
			done[i] = r
			mu.Lock()
			if opts.Recorder != nil {
				if err := opts.Recorder.UpdateEvalMember(ctx, storeMember(evalRunID, r, "")); err != nil {
					log.Printf("evals: record member %s: %v", r.RequestID, err)
				}
			}
			if opts.Progress != nil {
				opts.Progress(r)
			}
			mu.Unlock()
		}(i, r)
	}
	wg.Wait()

	report.Runs = done
	if opts.Recorder != nil {
		status := store.EvalStatusOK
		if ctx.Err() != nil {
			status = store.EvalStatusCancelled
		} else if allFailed(done) {
			status = store.EvalStatusFailed
		}
		if err := opts.Recorder.FinishEvalRun(context.WithoutCancel(ctx), evalRunID, status, time.Now().UTC()); err != nil {
			log.Printf("evals: close eval run %s: %v", evalRunID, err)
		}
	}
	return report, nil
}

// allFailed reports whether every run errored. A run where some members
// finished is an ok run with failed members, not a failed run: the comparison
// over what did finish still stands.
func allFailed(runs []Run) bool {
	for _, r := range runs {
		if r.Err == "" {
			return false
		}
	}
	return len(runs) > 0
}

// StoreMember is one Run as the store holds it, for a caller writing a
// rescored member back.
func StoreMember(evalRunID string, r Run) store.EvalMember {
	return storeMember(evalRunID, r, "")
}

// storeMember is one Run as the store holds it. status overrides the run's
// own, which is what records a member before it has one.
func storeMember(evalRunID string, r Run, status string) store.EvalMember {
	if status == "" {
		status = r.Status
		if status == "" {
			status = "failed"
		}
	}
	m := store.EvalMember{
		EvalRunID: evalRunID,
		RequestID: r.RequestID,
		TaskID:    r.TaskID,
		Variant:   r.Variant,
		Replicate: r.Replicate,
		SessionID: r.SessionID,
		Status:    status,
		CostUSD:   r.CostUSD,
		SubTurns:  r.SubTurns,
		Error:     r.Err,
	}
	if len(r.Scores) > 0 {
		if b, err := json.Marshal(r.Scores); err == nil {
			m.Scores = b
		}
	}
	if r.Verdict != nil {
		if b, err := json.Marshal(r.Verdict); err == nil {
			m.Verdict = b
		}
	}
	return m
}

// execute publishes one run and waits for it, filling in r.
func execute(ctx context.Context, pub Publisher, sessions Sessions, opts Options, task Task, r *Run) {
	runCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	if opts.MaxSubTurns > 0 {
		task.MaxSubTurns = opts.MaxSubTurns
	}
	if opts.Model != "" {
		task.Model = opts.Model
	}
	if opts.Effort != "" {
		task.Effort = opts.Effort
	}
	req := task.Request(r.RequestID, r.Variant)
	if err := req.Validate(); err != nil {
		r.Err = err.Error()
		return
	}
	if err := pub.Publish(runCtx, req); err != nil {
		r.Err = fmt.Sprintf("publish: %v", err)
		return
	}

	wr, err := waitForRequest(runCtx, sessions, r.RequestID, opts.PollInterval)
	if err != nil {
		r.Err = err.Error()
		return
	}
	r.SessionID = wr.SessionID
	r.Status = wr.Status
	if r.SessionID == "" {
		r.Err = "the request finished without ever starting a session"
		return
	}

	events, err := sessions.GetEvents(runCtx, r.SessionID)
	if err != nil {
		r.Err = fmt.Sprintf("read events: %v", err)
		return
	}
	r.Scores = Score(events)
	r.CostUSD, r.SubTurns = costAndSubTurns(events)

	if opts.Judge != nil {
		verdict, err := opts.Judge.Score(runCtx, opts.Suite.Rubric, events)
		if err != nil {
			// A missing verdict is not a failed run: the counters stand on
			// their own, and the judge is the part that needs a network call.
			log.Printf("evals: judge %s: %v", r.SessionID, err)
		} else {
			r.Verdict = verdict
		}
	}
}

// waitForRequest polls the work_requests row until it reaches a terminal
// status. The row is the queue's own record of the run, so this sees a
// request that failed before a session existed as well as one that finished.
func waitForRequest(ctx context.Context, sessions Sessions, requestID string, interval time.Duration) (store.WorkRequest, error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		wr, err := sessions.GetWorkRequest(ctx, requestID)
		if err == nil && wr.FinishedAt != nil {
			return wr, nil
		}
		select {
		case <-ctx.Done():
			return store.WorkRequest{}, fmt.Errorf("run did not finish before the eval timeout")
		case <-ticker.C:
		}
	}
}

// costAndSubTurns sums the session's usage events. Summing rather than taking
// the last one is what counts a sub-turn that was retried: the retry commits
// a second usage event for the same sub-turn and both are billed
// (internal/store/events.go).
func costAndSubTurns(events []store.Event) (float64, int) {
	var cost float64
	subTurns := 0
	for _, e := range events {
		if e.Kind != store.KindUsage {
			continue
		}
		var p store.UsagePayload
		if json.Unmarshal(e.Payload, &p) != nil {
			continue
		}
		cost += p.CostUSD
		if p.SubTurn > subTurns {
			subTurns = p.SubTurn
		}
	}
	return cost, subTurns
}

func newID(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("evals: crypto/rand unavailable: " + err.Error())
	}
	return prefix + "-" + hex.EncodeToString(b[:])
}
