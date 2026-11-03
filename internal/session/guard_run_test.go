package session

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/guard"
	"github.com/mrgeoffrich/agent-harness/internal/guard/guardtest"
	"github.com/mrgeoffrich/agent-harness/internal/store"
	"github.com/mrgeoffrich/agent-harness/internal/tools"
	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

type scriptedCall struct{ id, name, args string }

// modelScript answers each streaming request with the next turn: a round of
// tool calls, or a text answer when calls is empty. The last turn repeats.
type modelScript struct {
	mu    sync.Mutex
	turns [][]scriptedCall
	next  int
	seen  []string
}

func (m *modelScript) serve(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := new(strings.Builder)
		_, _ = fmt.Fprint(buf, readBody(r))
		m.mu.Lock()
		m.seen = append(m.seen, buf.String())
		i := m.next
		if i >= len(m.turns) {
			i = len(m.turns) - 1
		}
		m.next++
		calls := m.turns[i]
		m.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		if len(calls) == 0 {
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{Role: "assistant", Content: strPtr("done")}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishStop)}},
				Usage:   &wire.Usage{PromptTokens: 100, PromptCacheMissTokens: 100, CompletionTokens: 5},
			})
		} else {
			var deltas []wire.ToolCallDelta
			for idx, c := range calls {
				deltas = append(deltas, wire.ToolCallDelta{
					Index: idx, ID: c.id, Type: "function",
					Function: wire.ToolCallFuncDelta{Name: c.name, Arguments: c.args},
				})
			}
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{Role: "assistant", ToolCalls: deltas}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishToolCalls)}},
				Usage:   &wire.Usage{PromptTokens: 100, PromptCacheMissTokens: 100, CompletionTokens: 5},
			})
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(srv.Close)
	return srv
}

func readBody(r *http.Request) string {
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			return b.String()
		}
	}
}

func bashCall(id, command string) scriptedCall {
	args, _ := json.Marshal(map[string]string{"command": command})
	return scriptedCall{id: id, name: "bash", args: string(args)}
}

// guardRunner is a Runner whose guard talks to the fake stage servers.
func guardRunner(t *testing.T, model *httptest.Server, fake *guardtest.Fake) *Runner {
	t.Helper()
	r := newTestRunner(t, model.URL)
	bundle, err := guard.LoadBundle(guardtest.BundleJSON())
	if err != nil {
		t.Fatal(err)
	}
	r.Guards = func(setting store.GuardSetting) (GuardDecider, error) {
		return guard.NewPipeline(guard.PipelineConfig{
			Bundle:         bundle,
			Stage1Endpoint: setting.Stage1Endpoint,
			AnthropicKey:   "sk-ant-test",
			Stage2BaseURL:  fake.Stage2URL,
		})
	}
	return r
}

func guardSetting(mode string, fake *guardtest.Fake) store.GuardSetting {
	return store.GuardSetting{Mode: mode, Stage1Endpoint: fake.Stage1URL}
}

func runOptions(ws string, g store.GuardSetting) RunOptions {
	return RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "do the work", Guard: g,
	}
}

type loggedEvents struct{ events []store.Event }

func eventsOf(t *testing.T, r *Runner, id string) loggedEvents {
	t.Helper()
	events, err := r.Store.GetEvents(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return loggedEvents{events}
}

func (l loggedEvents) decisions(t *testing.T) []store.GuardDecisionPayload {
	t.Helper()
	var out []store.GuardDecisionPayload
	for _, e := range l.events {
		if e.Kind != store.KindGuardDecision {
			continue
		}
		var p store.GuardDecisionPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func (l loggedEvents) denials(t *testing.T) []store.ToolDeniedPayload {
	t.Helper()
	var out []store.ToolDeniedPayload
	for _, e := range l.events {
		if e.Kind != store.KindToolDenied {
			continue
		}
		var p store.ToolDeniedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func (l loggedEvents) results(t *testing.T) []store.ToolResultPayload {
	t.Helper()
	var out []store.ToolResultPayload
	for _, e := range l.events {
		if e.Kind != store.KindToolResult {
			continue
		}
		var p store.ToolResultPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func (l loggedEvents) guardUsage(t *testing.T) []store.UsagePayload {
	t.Helper()
	var out []store.UsagePayload
	for _, e := range l.events {
		if e.Kind != store.KindUsage {
			continue
		}
		var p store.UsagePayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.Model == guardtest.Stage2Model {
			out = append(out, p)
		}
	}
	return out
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func TestShadowRunsTheCommandAndRecordsTheDecision(t *testing.T) {
	fake := guardtest.New(t)
	model := (&modelScript{turns: [][]scriptedCall{{bashCall("call_a", "touch ran-a")}, {}}}).serve(t)
	r := guardRunner(t, model, fake)
	ws := t.TempDir()

	res, err := r.Run(t.Context(), runOptions(ws, guardSetting(store.GuardModeShadow, fake)))
	if err != nil {
		t.Fatal(err)
	}
	if !fileExists(filepath.Join(ws, "ran-a")) {
		t.Fatal("the command did not run")
	}
	logged := eventsOf(t, r, res.SessionID)
	d := logged.decisions(t)
	if len(d) != 1 {
		t.Fatalf("decisions = %d, want 1", len(d))
	}
	if d[0].ToolCallID != "call_a" || d[0].Mode != store.GuardModeShadow || d[0].Verdict != guard.VerdictAllow ||
		d[0].DecidedBy != guard.DecidedByStage2 || d[0].GuardVersion == "" || d[0].PBlock == nil {
		t.Fatalf("decision = %+v", d[0])
	}
	if usage := logged.guardUsage(t); len(usage) != 1 || usage[0].PromptCacheMissTokens != 120 {
		t.Fatalf("guard usage events = %+v", usage)
	}
	if got := logged.results(t); len(got) != 1 || got[0].IsError {
		t.Fatalf("results = %+v", got)
	}
	sess, err := r.Store.GetSession(t.Context(), res.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Guard != guardSetting(store.GuardModeShadow, fake) {
		t.Fatalf("stored setting = %+v", sess.Guard)
	}
}

// The stage 1 request is held, and the command still finishes first. The
// run's last event waits for the record.
func TestShadowDoesNotDelayTheCommand(t *testing.T) {
	fake := guardtest.New(t)
	hold := make(chan struct{})
	fake.Set(func(b *guardtest.Behaviour) { b.HoldStage1 = hold })
	model := (&modelScript{turns: [][]scriptedCall{{bashCall("call_a", "touch ran-a")}, {}}}).serve(t)
	r := guardRunner(t, model, fake)
	ws := t.TempDir()

	type outcome struct {
		res *RunResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := r.Run(t.Context(), runOptions(ws, guardSetting(store.GuardModeShadow, fake)))
		done <- outcome{res, err}
	}()

	select {
	case <-fake.Stage1Arrived:
	case <-time.After(20 * time.Second):
		t.Fatal("stage 1 was never asked")
	}
	deadline := time.Now().Add(20 * time.Second)
	for !fileExists(filepath.Join(ws, "ran-a")) {
		if time.Now().After(deadline) {
			t.Fatal("the command waited for the guard")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case o := <-done:
		t.Fatalf("the run ended before its guard record: %+v %v", o.res, o.err)
	case <-time.After(300 * time.Millisecond):
	}

	close(hold)
	o := <-done
	if o.err != nil {
		t.Fatal(o.err)
	}
	logged := eventsOf(t, r, o.res.SessionID)
	if len(logged.decisions(t)) != 1 {
		t.Fatal("the decision was not recorded before the run ended")
	}
	last := logged.events[len(logged.events)-1]
	if last.Kind != store.KindRunFinished {
		t.Fatalf("last event = %s", last.Kind)
	}
}

func TestShadowStageTwoOutageStillRunsTheCommandAndRecordsTheFailure(t *testing.T) {
	fake := guardtest.New(t)
	fake.Set(func(b *guardtest.Behaviour) { b.Stage2Status = http.StatusServiceUnavailable })
	model := (&modelScript{turns: [][]scriptedCall{{bashCall("call_a", "touch ran-a")}, {}}}).serve(t)
	r := guardRunner(t, model, fake)
	ws := t.TempDir()

	res, err := r.Run(t.Context(), runOptions(ws, guardSetting(store.GuardModeShadow, fake)))
	if err != nil {
		t.Fatal(err)
	}
	if !fileExists(filepath.Join(ws, "ran-a")) {
		t.Fatal("a stage 2 outage stopped the command")
	}
	d := eventsOf(t, r, res.SessionID).decisions(t)
	if len(d) != 1 || d[0].Verdict != guard.VerdictBlock || d[0].Stage2Error == "" || !d[0].Stage2Ran {
		t.Fatalf("decision = %+v", d)
	}
}

func TestShadowStageOneAllowNeverCallsStageTwo(t *testing.T) {
	fake := guardtest.New(t)
	fake.Set(func(b *guardtest.Behaviour) { b.PBlock = 0.01 })
	model := (&modelScript{turns: [][]scriptedCall{{bashCall("call_a", "ls")}, {}}}).serve(t)
	r := guardRunner(t, model, fake)

	res, err := r.Run(t.Context(), runOptions(t.TempDir(), guardSetting(store.GuardModeShadow, fake)))
	if err != nil {
		t.Fatal(err)
	}
	d := eventsOf(t, r, res.SessionID).decisions(t)
	if len(d) != 1 || d[0].DecidedBy != guard.DecidedByStage1 || d[0].Stage2Ran {
		t.Fatalf("decision = %+v", d)
	}
	if n := len(fake.Stage2Requests()); n != 0 {
		t.Fatalf("stage 2 saw %d requests", n)
	}
}

func TestGuardOffAndReadonlyScreenNothing(t *testing.T) {
	fake := guardtest.New(t)
	model := (&modelScript{turns: [][]scriptedCall{{bashCall("call_a", "touch ran-a")}, {}}}).serve(t)
	r := guardRunner(t, model, fake)

	res, err := r.Run(t.Context(), runOptions(t.TempDir(), store.GuardSetting{Mode: store.GuardModeOff}))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(eventsOf(t, r, res.SessionID).decisions(t)); n != 0 {
		t.Fatalf("an off guard recorded %d decisions", n)
	}

	opts := runOptions(t.TempDir(), guardSetting(store.GuardModeShadow, fake))
	opts.PermissionMode = tools.ModeReadOnly
	res, err = r.Run(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(eventsOf(t, r, res.SessionID).decisions(t)); n != 0 {
		t.Fatalf("a readonly session recorded %d decisions", n)
	}
	if n := len(fake.Stage1Requests()); n != 0 {
		t.Fatalf("stage 1 saw %d requests", n)
	}
}

func TestAGuardWithNoFactoryFailsTheRun(t *testing.T) {
	fake := guardtest.New(t)
	model := (&modelScript{turns: [][]scriptedCall{{}}}).serve(t)
	r := newTestRunner(t, model.URL)
	if _, err := r.Run(t.Context(), runOptions(t.TempDir(), guardSetting(store.GuardModeShadow, fake))); err == nil {
		t.Fatal("a run asking for a guard started on a runner with none")
	}
}

// Enforce cannot be selected by a create, so it is built directly here.
func TestEnforceBlocksWithoutRunningTheCommand(t *testing.T) {
	fake := guardtest.New(t)
	fake.Set(func(b *guardtest.Behaviour) { b.Verdict, b.Reason = "block", "SECRET-REASON sends data away" })
	script := &modelScript{turns: [][]scriptedCall{{bashCall("call_a", "touch ran-a")}, {}}}
	model := script.serve(t)
	r := guardRunner(t, model, fake)
	ws := t.TempDir()

	res, err := r.Run(t.Context(), runOptions(ws, guardSetting(store.GuardModeEnforce, fake)))
	if err != nil {
		t.Fatal(err)
	}
	if fileExists(filepath.Join(ws, "ran-a")) {
		t.Fatal("a blocked command ran")
	}
	logged := eventsOf(t, r, res.SessionID)
	denials := logged.denials(t)
	if len(denials) != 1 || denials[0].ToolCallID != "call_a" || denials[0].Name != "bash" {
		t.Fatalf("denials = %+v", denials)
	}
	if denials[0].Rule != "autonomy guard (stage 2)" {
		t.Fatalf("rule = %q", denials[0].Rule)
	}
	if strings.Contains(denials[0].Content, "SECRET-REASON") || strings.Contains(denials[0].Rule, "SECRET-REASON") {
		t.Fatalf("the refusal carries stage 2's reason: %+v", denials[0])
	}
	d := logged.decisions(t)
	if len(d) != 1 || d[0].Mode != store.GuardModeEnforce || d[0].Verdict != guard.VerdictBlock || d[0].Stage2Reason == "" {
		t.Fatalf("decision = %+v", d)
	}
	script.mu.Lock()
	last := script.seen[len(script.seen)-1]
	script.mu.Unlock()
	if !strings.Contains(last, "Denied by the autonomy guard") || strings.Contains(last, "SECRET-REASON") {
		t.Fatalf("the model's next request does not carry the refusal alone: %s", last)
	}
}

func TestEnforceAllowRunsTheCommand(t *testing.T) {
	fake := guardtest.New(t)
	model := (&modelScript{turns: [][]scriptedCall{{bashCall("call_a", "touch ran-a")}, {}}}).serve(t)
	r := guardRunner(t, model, fake)
	ws := t.TempDir()

	res, err := r.Run(t.Context(), runOptions(ws, guardSetting(store.GuardModeEnforce, fake)))
	if err != nil {
		t.Fatal(err)
	}
	if !fileExists(filepath.Join(ws, "ran-a")) {
		t.Fatal("an allowed command did not run")
	}
	if n := len(eventsOf(t, r, res.SessionID).denials(t)); n != 0 {
		t.Fatalf("denials = %d", n)
	}
}

// Rules block without a model, and the refusal names them.
func TestEnforceRuleBlockNamesTheRules(t *testing.T) {
	fake := guardtest.New(t)
	model := (&modelScript{turns: [][]scriptedCall{{bashCall("call_a", "elevate true")}, {}}}).serve(t)
	r := guardRunner(t, model, fake)

	res, err := r.Run(t.Context(), runOptions(t.TempDir(), guardSetting(store.GuardModeEnforce, fake)))
	if err != nil {
		t.Fatal(err)
	}
	denials := eventsOf(t, r, res.SessionID).denials(t)
	if len(denials) != 1 || denials[0].Rule != "autonomy guard (rules: elevate)" {
		t.Fatalf("denials = %+v", denials)
	}
	if n := len(fake.Stage1Requests()) + len(fake.Stage2Requests()); n != 0 {
		t.Fatalf("a rule block called a model %d times", n)
	}
}

// Results append in tool_calls order whichever check finishes first.
func TestParallelChecksAppendInCallOrder(t *testing.T) {
	fake := guardtest.New(t)
	slow := make(chan struct{})
	fake.Set(func(b *guardtest.Behaviour) {
		b.PBlock = 0.01
		b.HoldCommand = map[string]chan struct{}{"touch ran-a": slow}
	})
	script := &modelScript{turns: [][]scriptedCall{{bashCall("call_a", "touch ran-a"), bashCall("call_b", "touch ran-b")}, {}}}
	r := guardRunner(t, script.serve(t), fake)
	ws := t.TempDir()

	go func() {
		// The second call's check has finished by the time the first is let go.
		deadline := time.Now().Add(20 * time.Second)
		for len(fake.Stage1Requests()) < 2 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		time.Sleep(100 * time.Millisecond)
		close(slow)
	}()
	res, err := r.Run(t.Context(), runOptions(ws, guardSetting(store.GuardModeEnforce, fake)))
	if err != nil {
		t.Fatal(err)
	}
	logged := eventsOf(t, r, res.SessionID)
	got := logged.results(t)
	if len(got) != 2 || got[0].ToolCallID != "call_a" || got[1].ToolCallID != "call_b" {
		t.Fatalf("results = %+v", got)
	}
	if len(logged.decisions(t)) != 2 {
		t.Fatalf("decisions = %d", len(logged.decisions(t)))
	}
}

func TestResumeTakesTheCreatesMode(t *testing.T) {
	fake := guardtest.New(t)
	fake.Set(func(b *guardtest.Behaviour) { b.Verdict = "block" })
	script := &modelScript{turns: [][]scriptedCall{
		{bashCall("call_a", "touch ran-a")}, {},
		{bashCall("call_b", "touch ran-b")}, {},
		{bashCall("call_c", "touch ran-c")}, {},
		{bashCall("call_d", "touch ran-d")}, {},
	}}
	r := guardRunner(t, script.serve(t), fake)
	ws := t.TempDir()

	res, err := r.Run(t.Context(), runOptions(ws, guardSetting(store.GuardModeShadow, fake)))
	if err != nil {
		t.Fatal(err)
	}
	id := res.SessionID
	ctx := t.Context()

	// No setting named: the stored shadow keeps screening, and does not refuse.
	if _, err := r.Resume(ctx, ResumeOptions{SessionID: id, Prompt: "again", MaxTokens: 4000}); err != nil {
		t.Fatal(err)
	}
	if !fileExists(filepath.Join(ws, "ran-b")) {
		t.Fatal("a resumed shadow session refused a command")
	}
	if n := len(eventsOf(t, r, id).decisions(t)); n != 2 {
		t.Fatalf("decisions after a plain resume = %d, want 2", n)
	}

	// A changed mode from the resume replaces the stored one and is stored.
	enforce := guardSetting(store.GuardModeEnforce, fake)
	if _, err := r.Resume(ctx, ResumeOptions{SessionID: id, Prompt: "third", MaxTokens: 4000, Guard: &enforce}); err != nil {
		t.Fatal(err)
	}
	if fileExists(filepath.Join(ws, "ran-c")) {
		t.Fatal("the resumed enforce guard let a blocked command run")
	}
	if sess, _ := r.Store.GetSession(ctx, id); sess.Guard.Mode != store.GuardModeEnforce {
		t.Fatalf("stored mode = %q", sess.Guard.Mode)
	}

	// Naming off turns it off, and that is stored too.
	off := store.GuardSetting{Mode: store.GuardModeOff}
	if _, err := r.Resume(ctx, ResumeOptions{SessionID: id, Prompt: "fourth", MaxTokens: 4000, Guard: &off}); err != nil {
		t.Fatal(err)
	}
	if !fileExists(filepath.Join(ws, "ran-d")) {
		t.Fatal("an off guard refused a command")
	}
	if n := len(eventsOf(t, r, id).decisions(t)); n != 3 {
		t.Fatalf("decisions = %d, want 3", n)
	}
}

func TestSubagentBashCallsAreScreened(t *testing.T) {
	fake := guardtest.New(t)
	task := scriptedCall{id: "call_task", name: "task", args: `{"description":"look","prompt":"find it","subagent_type":"general"}`}
	script := &modelScript{turns: [][]scriptedCall{
		{task},                                  // parent
		{bashCall("call_sub", "touch ran-sub")}, // subagent
		{},                                      // subagent answers
		{},                                      // parent answers
	}}
	r := guardRunner(t, script.serve(t), fake)
	ws := t.TempDir()

	res, err := r.Run(t.Context(), runOptions(ws, guardSetting(store.GuardModeShadow, fake)))
	if err != nil {
		t.Fatal(err)
	}
	if !fileExists(filepath.Join(ws, "ran-sub")) {
		t.Fatal("the subagent's command did not run")
	}
	var childID string
	for _, p := range eventsOf(t, r, res.SessionID).results(t) {
		if p.Name == "task" {
			childID = p.ChildSessionID
		}
	}
	if childID == "" {
		t.Fatal("no child session")
	}
	d := eventsOf(t, r, childID).decisions(t)
	if len(d) != 1 || d[0].ToolCallID != "call_sub" || d[0].Mode != store.GuardModeShadow {
		t.Fatalf("the subagent's decisions = %+v", d)
	}
	if child, _ := r.Store.GetSession(t.Context(), childID); child.Guard.Mode != store.GuardModeShadow {
		t.Fatalf("the subagent's stored setting = %+v", child.Guard)
	}
}

func TestCompactionSuccessorKeepsTheGuardSetting(t *testing.T) {
	fake := guardtest.New(t)
	model := plainAnswerServer(t, "done")
	defer model.Close()
	r := guardRunner(t, model, fake)
	setting := guardSetting(store.GuardModeShadow, fake)

	res, err := r.Run(t.Context(), runOptions(t.TempDir(), setting))
	if err != nil {
		t.Fatal(err)
	}
	sess, err := r.Store.GetSession(t.Context(), res.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	events, err := r.Store.GetEvents(t.Context(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	successor, _, err := r.compact(t.Context(), sess, events, sess.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if successor.ID == sess.ID || successor.Guard != setting {
		t.Fatalf("successor setting = %+v", successor.Guard)
	}
}

// A cancelled run leaves its shadow check to finish, and the record is
// committed on a context the cancellation does not reach.
func TestShadowRecordSurvivesACancelledRun(t *testing.T) {
	fake := guardtest.New(t)
	hold := make(chan struct{})
	fake.Set(func(b *guardtest.Behaviour) { b.HoldStage1 = hold })
	model := (&modelScript{turns: [][]scriptedCall{{bashCall("call_a", "touch ran-a")}, {}}}).serve(t)
	r := guardRunner(t, model, fake)
	ws := t.TempDir()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan *RunResult, 1)
	go func() {
		res, _ := r.Run(ctx, runOptions(ws, guardSetting(store.GuardModeShadow, fake)))
		done <- res
	}()
	select {
	case <-fake.Stage1Arrived:
	case <-time.After(20 * time.Second):
		t.Fatal("stage 1 was never asked")
	}
	for deadline := time.Now().Add(20 * time.Second); !fileExists(filepath.Join(ws, "ran-a")) && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	res := <-done
	if res == nil {
		t.Fatal("no result from the cancelled run")
	}
	close(hold)

	deadline := time.Now().Add(20 * time.Second)
	for {
		if len(eventsOf(t, r, res.SessionID).decisions(t)) == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the decision was never recorded; run status %s", res.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
