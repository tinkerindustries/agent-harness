package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

type fakeGuard struct {
	mu      sync.Mutex
	calls   []GuardCall
	refusal *GuardRefusal
}

func (g *fakeGuard) Screen(_ context.Context, call GuardCall) *GuardRefusal {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, call)
	return g.refusal
}

func guardExecutor(t *testing.T, mode Mode, g Guard) (*Executor, string) {
	t.Helper()
	ws := t.TempDir()
	e, err := NewExecutor(ws, &Policy{Mode: mode})
	if err != nil {
		t.Fatal(err)
	}
	e.Guard = g
	return e, ws
}

func bashToolCall(id, args string) wire.ToolCall {
	return wire.ToolCall{ID: id, Type: "function", Function: wire.ToolCallFunc{Name: "bash", Arguments: args}}
}

func TestGuardRefusalIsADenialAndTheCommandDoesNotRun(t *testing.T) {
	g := &fakeGuard{refusal: &GuardRefusal{Rule: "autonomy guard (stage 2)", Content: "Denied by the autonomy guard (stage 2). The command was not run."}}
	e, ws := guardExecutor(t, ModeFull, g)

	out := e.Execute(context.Background(), bashToolCall("call_1", `{"command":"touch ran","workdir":"."}`))
	if !out.Denied || out.Rule != "autonomy guard (stage 2)" || !out.Result.IsError || out.Name != "bash" {
		t.Fatalf("outcome = %+v", out)
	}
	if !strings.Contains(out.Result.Content, "autonomy guard") {
		t.Fatalf("content = %q", out.Result.Content)
	}
	if _, err := os.Stat(filepath.Join(ws, "ran")); err == nil {
		t.Fatal("the refused command ran")
	}
	if len(g.calls) != 1 || g.calls[0] != (GuardCall{ID: "call_1", Command: "touch ran", Workdir: e.Workspace}) {
		t.Fatalf("calls = %+v", g.calls)
	}
}

func TestGuardNilRefusalLetsTheCommandRun(t *testing.T) {
	g := &fakeGuard{}
	e, ws := guardExecutor(t, ModeFull, g)
	out := e.Execute(context.Background(), bashToolCall("call_1", `{"command":"touch ran"}`))
	if out.Denied || out.Result.IsError {
		t.Fatalf("outcome = %+v", out)
	}
	if _, err := os.Stat(filepath.Join(ws, "ran")); err != nil {
		t.Fatal("the allowed command did not run")
	}
}

func TestGuardScreensOnlyBashUnderFull(t *testing.T) {
	g := &fakeGuard{refusal: &GuardRefusal{Rule: "autonomy guard (stage 2)"}}

	e, _ := guardExecutor(t, ModeReadOnly, g)
	if out := e.Execute(context.Background(), bashToolCall("c", `{"command":"true"}`)); !out.Denied || !strings.Contains(out.Result.Content, "permission policy") {
		t.Fatalf("readonly bash = %+v", out)
	}
	if len(g.calls) != 0 {
		t.Fatalf("the guard saw a readonly call: %+v", g.calls)
	}

	e, ws := guardExecutor(t, ModeFull, g)
	if err := os.WriteFile(filepath.Join(ws, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	read := wire.ToolCall{ID: "r", Type: "function", Function: wire.ToolCallFunc{Name: "read", Arguments: `{"file_path":"f.txt"}`}}
	if out := e.Execute(context.Background(), read); out.Denied {
		t.Fatalf("read = %+v", out)
	}
	if out := e.Execute(context.Background(), bashToolCall("c", `{"command":""}`)); out.Denied {
		t.Fatalf("an empty command was screened: %+v", out)
	}
	if len(g.calls) != 0 {
		t.Fatalf("the guard saw %+v", g.calls)
	}
}
