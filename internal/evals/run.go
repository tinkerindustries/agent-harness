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

// A Report is everything one `harness eval run` produced.
type Report struct {
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
	// Judge, when set, scores each finished transcript.
	Judge *Judge
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

	report := &Report{
		Suite:      opts.Suite.Name,
		Variants:   opts.Variants,
		Replicates: opts.Replicates,
		StartedAt:  time.Now().UTC(),
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
					RequestID: newRequestID(),
				})
			}
		}
	}

	tasksByID := map[string]Task{}
	for _, t := range opts.Suite.Tasks {
		tasksByID[t.ID] = t
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
			if opts.Progress != nil {
				mu.Lock()
				opts.Progress(r)
				mu.Unlock()
			}
		}(i, r)
	}
	wg.Wait()

	report.Runs = done
	return report, nil
}

// execute publishes one run and waits for it, filling in r.
func execute(ctx context.Context, pub Publisher, sessions Sessions, opts Options, task Task, r *Run) {
	runCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

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

func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("evals: crypto/rand unavailable: " + err.Error())
	}
	return "eval-" + hex.EncodeToString(b[:])
}
