package stdiosession

import (
	"strings"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/session"
	"github.com/mrgeoffrich/agent-harness/internal/tools"
)

// TestFollowCompactionAliasesTheSuccessor pins the server half of the
// compaction handover: the successor session id must reach the same registry
// and host the run used, so a shell started after the fork is addressable by
// the successor's runs and a completion wakes the successor.
func TestFollowCompactionAliasesTheSuccessor(t *testing.T) {
	s := NewServer(Options{})
	shells := tools.NewShells("sess-original", nil)
	host := &hostTools{}
	s.shells["sess-original"] = shells
	s.hosts["sess-original"] = host

	got := s.followCompaction("sess-original", &session.RunResult{SessionID: "sess-child"}, shells)
	if got != "sess-child" {
		t.Fatalf("followCompaction returned %q, want the successor", got)
	}
	if s.shells["sess-child"] != shells {
		t.Fatal("the successor does not resolve to the run's shell registry")
	}
	if s.hosts["sess-child"] != host {
		t.Fatal("the successor does not resolve to the run's tool provider")
	}

	// A run that did not compact keeps its own id.
	if got := s.followCompaction("sess-original", &session.RunResult{SessionID: "sess-original"}, shells); got != "sess-original" {
		t.Fatalf("an uncompacted run returned %q, want its own id", got)
	}
}

// TestBackgroundShellCompletionWakesTheSession is fix two end to end: a Bash
// call starts a shell, the run ends, and when the shell exits the server must
// start a second response on its own with the completion as its input. Before
// this, the shell was killed at the end of the run and nobody was ever told.
func TestBackgroundShellCompletionWakesTheSession(t *testing.T) {
	f := newFixture(t,
		callThen("call_bash", "bash", `{"command":"sleep 0.2; echo finished","run_in_background":true}`),
		answer("started"),
		answer("the shell finished"),
	)

	var created CreateResult
	f.client.handshake(ClientCapabilities{})
	p := f.createParams("run it")
	// Bash is refused in the fixture's default read-only mode, and a denied
	// call never starts a shell.
	p.Harness.PermissionMode = "full"
	if rerr := f.client.call(MethodResponsesCreate, p, &created); rerr != nil {
		t.Fatalf("create: %s", rerr.Message)
	}

	first := f.client.waitFor(NotifyResponseCompleted)
	if got := textOf(first, ItemMessage); got != "started" {
		t.Fatalf("first response answer = %q, want %q", got, "started")
	}

	// The shell exits after the run has finished. Nothing the client sends
	// starts the next response; the completion does.
	second := f.client.waitFor(NotifyResponseCompleted)

	var notice string
	for _, s := range steps(second) {
		if s.Item.Type != ItemMessage || s.Item.Role != "user" || s.Item.Harness == nil {
			continue
		}
		if s.Item.Harness.Source != "background" {
			continue
		}
		for _, c := range s.Item.Content {
			notice += c.Text
		}
	}
	if !strings.Contains(notice, "Background shell bash_1") {
		t.Fatalf("the wake's input is not the completion notice: %q (steps %v)", notice, stepTypes(second))
	}
	if !strings.Contains(notice, "finished with exit code 0") {
		t.Fatalf("the notice does not carry the exit code: %q", notice)
	}
	if got := textOf(second, ItemMessage); got != "the shell finished" {
		t.Fatalf("second response answer = %q, want %q", got, "the shell finished")
	}
}
