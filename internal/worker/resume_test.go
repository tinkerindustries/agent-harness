package worker

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mrgeoffrich/deepseek-harness/internal/queue"
	"github.com/mrgeoffrich/deepseek-harness/internal/session"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
	"github.com/mrgeoffrich/deepseek-harness/internal/workspace"
)

// recordingRunner answers a resume and records what it was asked for, and
// fails loudly on the fresh-run path: the point of every test here is that a
// resume reaches Resume without touching Create, the attachments or the
// clone (docs/RUN-CONTROL.md "Continuing").
type recordingRunner struct {
	mu      sync.Mutex
	resumed []session.ResumeOptions
	created int
	ran     int
}

func (r *recordingRunner) Create(context.Context, session.RunOptions) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.created++
	return nil
}

func (r *recordingRunner) FailSetup(context.Context, string, error) error { return nil }

func (r *recordingRunner) Run(context.Context, session.RunOptions) (*session.RunResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ran++
	return &session.RunResult{Status: store.StatusOK}, nil
}

func (r *recordingRunner) Resume(_ context.Context, opts session.ResumeOptions) (*session.RunResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resumed = append(r.resumed, opts)
	return &session.RunResult{SessionID: opts.SessionID, Status: store.StatusOK, Text: "continued", SubTurns: 3}, nil
}

func (r *recordingRunner) snapshot() ([]session.ResumeOptions, int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]session.ResumeOptions(nil), r.resumed...), r.created, r.ran
}

// A request naming a session continues it: Resume is called with that
// session id and the request's prompt, and none of the preparation window
// runs — no row created, no workspace built.
func TestPoolResumesTheNamedSession(t *testing.T) {
	runner := &recordingRunner{}
	h := newTestHarnessWithRunner(t, "", 1, func(*store.Store) Runner { return runner })
	// A resume must never prepare a workspace. Panicking is the assertion:
	// there is no session id here that PrepareWorkspace could even be given
	// a sensible directory for.
	h.pool.PrepareWorkspace = func(context.Context, string, string, []queue.Repo, []workspace.Attachment) (string, error) {
		t.Error("PrepareWorkspace was called for a resume; a resumed session keeps the workspace it already has")
		return "", nil
	}
	stop := h.startPool(t)
	defer stop()

	h.publish(t, queue.Request{
		RequestID:       "req-resume",
		ResumeSessionID: "sess-existing",
		Prompt:          "now change the front panel",
		PermissionMode:  "full",
		MaxSubTurns:     11,
	})

	res := h.fetchFinalResult(t, "req-resume", 5*time.Second)
	if res.Status != queue.StatusOK {
		t.Fatalf("result status = %q, want ok (error %+v)", res.Status, res.Error)
	}
	// The resumed session's own id, not a freshly minted one: it is what the
	// request's row records and what a stop would have to name.
	if res.SessionID != "sess-existing" {
		t.Fatalf("result session id = %q, want the session the request named", res.SessionID)
	}

	resumed, created, ran := runner.snapshot()
	if len(resumed) != 1 {
		t.Fatalf("Resume called %d times, want 1", len(resumed))
	}
	if resumed[0].SessionID != "sess-existing" {
		t.Errorf("Resume session id = %q, want sess-existing", resumed[0].SessionID)
	}
	if resumed[0].Prompt != "now change the front panel" {
		t.Errorf("Resume prompt = %q, want the request's prompt", resumed[0].Prompt)
	}
	if resumed[0].MaxSubTurns != 11 {
		t.Errorf("Resume max sub-turns = %d, want the request's 11", resumed[0].MaxSubTurns)
	}
	if resumed[0].MaxTokens != 4000 {
		t.Errorf("Resume max tokens = %d, want the pool's default 4000", resumed[0].MaxTokens)
	}
	if created != 0 || ran != 0 {
		t.Errorf("Create/Run called %d/%d times on a resume, want 0/0", created, ran)
	}
}

// The work_requests row carries the resumed session, so a second run of one
// session is traceable to the request that asked for it — and deepseek_result
// answers for it the way it answers for any other request.
func TestPoolAttachesTheResumedSessionToTheRequestRow(t *testing.T) {
	runner := &recordingRunner{}
	h := newTestHarnessWithRunner(t, "", 1, func(*store.Store) Runner { return runner })
	stop := h.startPool(t)
	defer stop()

	h.publish(t, queue.Request{
		RequestID:       "req-resume-row",
		ResumeSessionID: "sess-existing",
		Prompt:          "keep going",
		PermissionMode:  "full",
	})

	row := h.finalRowTerminal(t, "req-resume-row", 5*time.Second)
	if row.SessionID != "sess-existing" {
		t.Fatalf("work request session id = %q, want sess-existing", row.SessionID)
	}
}
